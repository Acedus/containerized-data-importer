//Licensed under the Apache License, Version 2.0 (the "License");
//you may not use this file except in compliance with the License.
//You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
//Unless required by applicable law or agreed to in writing, software
//distributed under the License is distributed on an "AS IS" BASIS,
//WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//See the License for the specific language governing permissions and
//limitations under the License.

// Pushes an OCI artifact from an oci-archive to the functional test registry. The images the
// registry serves are built by buildah, which only produces container images, and the higher
// level containers/image copy API refuses an artifact whose configuration is not an image
// configuration, so the blobs and manifests are copied one by one instead.
package main

import (
	"context"
	"flag"

	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/oci/archive"
	"github.com/containers/image/v5/pkg/blobinfocache"
	"github.com/containers/image/v5/types"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"

	"k8s.io/klog/v2"
)

func main() {
	src := flag.String("src", "", "path of the oci-archive holding the artifact")
	dest := flag.String("dest", "", "registry reference to push the artifact to, without a transport")
	klog.InitFlags(nil)
	flag.Parse()

	if err := push(context.Background(), *src, *dest); err != nil {
		klog.Fatal(errors.Wrapf(err, "pushing %s to %s errored: ", *src, *dest))
	}
	klog.Infof("Pushed %s to %s", *src, *dest)
}

func push(ctx context.Context, srcPath, destRef string) error {
	srcRef, err := archive.ParseReference(srcPath)
	if err != nil {
		return errors.Wrap(err, "parsing the source")
	}
	dstRef, err := docker.ParseReference("//" + destRef)
	if err != nil {
		return errors.Wrap(err, "parsing the destination")
	}
	// the registry serves a certificate of its own
	sys := &types.SystemContext{DockerInsecureSkipTLSVerify: types.NewOptionalBool(true)}

	src, err := srcRef.NewImageSource(ctx, sys)
	if err != nil {
		return errors.Wrap(err, "opening the source")
	}
	defer src.Close()

	dst, err := dstRef.NewImageDestination(ctx, sys)
	if err != nil {
		return errors.Wrap(err, "opening the destination")
	}
	defer dst.Close()

	cache := blobinfocache.DefaultCache(sys)
	manBlob, mimeType, err := src.GetManifest(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "reading the manifest")
	}

	if manifest.MIMETypeIsMultiImage(mimeType) {
		list, err := manifest.ListFromBlob(manBlob, mimeType)
		if err != nil {
			return errors.Wrap(err, "parsing the image index")
		}
		for _, instance := range list.Instances() {
			if err := pushInstance(ctx, src, dst, cache, &instance); err != nil {
				return err
			}
		}
		// the index goes last, it references the manifests pushed above
		if err := dst.PutManifest(ctx, manBlob, nil); err != nil {
			return errors.Wrap(err, "writing the image index")
		}
	} else if err := pushInstance(ctx, src, dst, cache, nil); err != nil {
		return err
	}

	return dst.Commit(ctx, image.UnparsedInstance(src, nil))
}

// pushInstance copies one manifest of the artifact, blobs first: a registry rejects a manifest
// referencing blobs it does not hold yet.
func pushInstance(ctx context.Context, src types.ImageSource, dst types.ImageDestination, cache types.BlobInfoCache, instanceDigest *digest.Digest) error {
	manBlob, mimeType, err := src.GetManifest(ctx, instanceDigest)
	if err != nil {
		return errors.Wrap(err, "reading the manifest")
	}
	if mimeType != imgspecv1.MediaTypeImageManifest {
		return errors.Errorf("expected an OCI image manifest, got %q", mimeType)
	}
	man, err := manifest.OCI1FromManifest(manBlob)
	if err != nil {
		return errors.Wrap(err, "parsing the manifest")
	}

	if err := pushBlob(ctx, src, dst, cache, man.Config, true); err != nil {
		return err
	}
	for _, layer := range man.Layers {
		if err := pushBlob(ctx, src, dst, cache, layer, false); err != nil {
			return err
		}
	}

	if err := dst.PutManifest(ctx, manBlob, instanceDigest); err != nil {
		return errors.Wrap(err, "writing the manifest")
	}
	return nil
}

func pushBlob(ctx context.Context, src types.ImageSource, dst types.ImageDestination, cache types.BlobInfoCache, desc imgspecv1.Descriptor, isConfig bool) error {
	info := manifest.BlobInfoFromOCI1Descriptor(desc)
	reader, _, err := src.GetBlob(ctx, info, cache)
	if err != nil {
		return errors.Wrapf(err, "reading blob %v", info.Digest)
	}
	defer reader.Close()

	if _, err := dst.PutBlob(ctx, reader, info, cache, isConfig); err != nil {
		return errors.Wrapf(err, "writing blob %v", info.Digest)
	}
	return nil
}
