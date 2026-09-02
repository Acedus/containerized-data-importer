/*
Copyright 2018 The CDI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/oci/archive"
	"github.com/containers/image/v5/pkg/blobinfocache"
	"github.com/containers/image/v5/types"

	"k8s.io/klog/v2"

	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
)

const (
	bootcImageLabel   = "containers.bootc"
	ostreeBootLabel   = "ostree.bootable"
	bootcLabelEnabled = "1"
)

// ErrBootcImageDetected is returned when a bootc/ostree-bootable container image is detected
// but conversion is not yet implemented.
var ErrBootcImageDetected = errors.New("bootc image detected: this image contains an ostree-based bootable OS (containers.bootc=1 or ostree.bootable=1) and cannot be imported as a regular container disk; bootc-to-disk conversion is not yet implemented")

// registryImage is a container image in a registry together with everything needed to
// reach it. It is the whole registry surface CDI needs: the datasource pulls a disk
// image out of one, the DataImportCron poller only asks one for its digest.
type registryImage struct {
	endpoint string
	sys      *types.SystemContext
}

// registryOption configures how the registry holding an image is reached.
type registryOption func(*types.SystemContext)

// newRegistryImage describes the image at endpoint, a colon-separated
// transport:reference pair.
func newRegistryImage(endpoint string, opts ...registryOption) registryImage {
	sys := &types.SystemContext{}
	for _, opt := range opts {
		opt(sys)
	}
	return registryImage{endpoint: endpoint, sys: sys}
}

// withCredentials authenticates against the registry. Both halves are required.
func withCredentials(accessKey, secKey string) registryOption {
	return func(sys *types.SystemContext) {
		if accessKey == "" || secKey == "" {
			return
		}
		sys.DockerAuthConfig = &types.DockerAuthConfig{
			Username: accessKey,
			Password: secKey,
		}
	}
}

// withCertDir verifies the registry identity against the public CA keys in dir.
func withCertDir(dir string) registryOption {
	return func(sys *types.SystemContext) {
		if dir == "" {
			return
		}
		sys.DockerCertPath = dir
		sys.DockerDaemonCertPath = dir
	}
}

// withInsecureTLS allows an insecure registry.
func withInsecureTLS(insecure bool) registryOption {
	return func(sys *types.SystemContext) {
		if !insecure {
			return
		}
		sys.DockerDaemonInsecureSkipTLSVerify = true
		sys.DockerInsecureSkipTLSVerify = types.NewOptionalBool(true)
	}
}

// withArchitecture picks the instance to import out of a multi-arch image index.
func withArchitecture(architecture string) registryOption {
	return func(sys *types.SystemContext) {
		sys.ArchitectureChoice = architecture
	}
}

// digest returns the digest of the image manifest.
func (ri registryImage) digest(ctx context.Context) (string, error) {
	klog.Infof("Inspecting image from '%v'", ri.endpoint)

	src, err := ri.open(ctx)
	if err != nil {
		return "", err
	}
	defer closeImage(src)

	rawManifest, _, err := src.GetManifest(ctx, nil)
	if err != nil {
		return "", err
	}

	imageDigest, err := manifest.Digest(rawManifest)
	if err != nil {
		return "", err
	}

	return imageDigest.String(), nil
}

// copyFile downloads the image and hands its layers to e, which writes out the file it
// is looking for. It returns the inspected image info.
func (ri registryImage) copyFile(ctx context.Context, e fileExtractor) (*types.ImageInspectInfo, error) {
	klog.Infof("Downloading image from '%v', copying file from '%v' to '%v'", ri.endpoint, e.pathPrefix, e.destDir)

	src, err := ri.open(ctx)
	if err != nil {
		return nil, err
	}

	// image.FromSource takes ownership of src and closes it with the image, so src
	// is only closed here when FromSource itself fails.
	img, err := image.FromSource(ctx, ri.sys, src)
	if err != nil {
		closeImage(src)
		klog.Errorf("Error retrieving image: %v", err)
		return nil, fmt.Errorf("Error retrieving image: %w", err)
	}
	defer closeImage(img)

	// The config the checks below read is also what the caller gets back, so inspect once.
	info, err := img.Inspect(ctx)
	if err != nil {
		return nil, fmt.Errorf("Error inspecting image: %w", err)
	}
	if err := ri.validate(info); err != nil {
		return nil, err
	}

	cache := blobinfocache.DefaultCache(ri.sys)
	openBlob := func(ctx context.Context, layer types.BlobInfo) (io.ReadCloser, error) {
		blob, _, err := src.GetBlob(ctx, layer, cache)
		return blob, err
	}
	if err := e.extract(ctx, img.LayerInfos(), openBlob); err != nil {
		return nil, err
	}

	return info, nil
}

// validate rejects an image CDI cannot import as a container disk.
func (ri registryImage) validate(info *types.ImageInspectInfo) error {
	if err := validateImagePlatformMatch(ri.sys, info); err != nil {
		return err
	}
	return checkBootcImage(info)
}

// open resolves the endpoint and opens the image for reading. The caller owns the
// returned source and must close it.
func (ri registryImage) open(ctx context.Context) (types.ImageSource, error) {
	ref, err := parseImageName(ri.endpoint)
	if err != nil {
		klog.Errorf("Could not parse image: %v", err)
		return nil, fmt.Errorf("Could not parse image: %w", err)
	}

	src, err := ref.NewImageSource(ctx, ri.sys)
	if err != nil {
		klog.Errorf("Could not create image reference: %v", err)
		return nil, NewImagePullFailedError(err)
	}
	return src, nil
}

// checkBootcImage rejects an image holding an ostree-based bootable OS rather than a
// container disk.
func checkBootcImage(info *types.ImageInspectInfo) error {
	if info.Labels[bootcImageLabel] == bootcLabelEnabled ||
		info.Labels[ostreeBootLabel] == bootcLabelEnabled {
		klog.Infof("Detected bootc/ostree-bootable container image")
		return ErrBootcImageDetected
	}
	return nil
}

// validateImagePlatformMatch asserts that the image matches the requested architecture.
// containers/image already picks the matching instance out of an image index, but a
// single-platform manifest offers no choice, so it has to be checked here.
func validateImagePlatformMatch(sys *types.SystemContext, info *types.ImageInspectInfo) error {
	if sys.ArchitectureChoice == "" || info.Architecture == sys.ArchitectureChoice {
		return nil
	}
	return fmt.Errorf(`Error validating architecture: manifest image architecture: "%s" doesn't match requested architecture: "%s"`,
		info.Architecture, sys.ArchitectureChoice)
}

func parseImageName(img string) (types.ImageReference, error) {
	scheme, ref, found := strings.Cut(img, ":")
	if !found {
		return nil, fmt.Errorf(`Invalid image name "%s", expected colon-separated transport:reference`, img)
	}

	switch scheme {
	case cdiv1.RegistrySchemeDocker:
		return docker.ParseReference(ref)
	case cdiv1.RegistrySchemeOci:
		return archive.ParseReference(ref)
	default:
		return nil, fmt.Errorf(`Invalid image name "%s", unknown transport`, img)
	}
}

func closeImage(c io.Closer) {
	if err := c.Close(); err != nil {
		klog.Warningf("Could not close image source: %v ", err)
	}
}

// GetImageDigest returns the digest of the container image at url.
// url: source registry url.
// accessKey: accessKey for the registry described in url.
// secKey: secretKey for the registry described in url.
// certDir: directory public CA keys are stored for registry identity verification
// insecureRegistry: boolean if true will allow insecure registries.
func GetImageDigest(url, accessKey, secKey, certDir string, insecureRegistry bool) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	return newRegistryImage(url,
		withCredentials(accessKey, secKey),
		withCertDir(certDir),
		withInsecureTLS(insecureRegistry),
	).digest(ctx)
}
