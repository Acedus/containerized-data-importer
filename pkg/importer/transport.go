/*
Copyright 2020 The CDI Authors.

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
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
)

const (
	whFilePrefix = ".wh."
)

var (
	errReadingLayer = errors.New("Error reading layer")

	// ErrBootcImageDetected is returned when a bootc/ostree-bootable container image is detected
	// but conversion is not yet implemented.
	ErrBootcImageDetected = errors.New("bootc image detected: this image contains an ostree-based bootable OS (containers.bootc=1 or ostree.bootable=1) and cannot be imported as a regular container disk; bootc-to-disk conversion is not yet implemented")
)

func commandTimeoutContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func buildSourceContext(accessKey, secKey, imageArchitecture, certDir string, insecureRegistry bool) *types.SystemContext {
	ctx := &types.SystemContext{}
	if accessKey != "" && secKey != "" {
		ctx.DockerAuthConfig = &types.DockerAuthConfig{
			Username: accessKey,
			Password: secKey,
		}
	}
	if certDir != "" {
		ctx.DockerCertPath = certDir
		ctx.DockerDaemonCertPath = certDir
	}

	if insecureRegistry {
		ctx.DockerDaemonInsecureSkipTLSVerify = true
		ctx.DockerInsecureSkipTLSVerify = types.NewOptionalBool(true)
	}

	if imageArchitecture != "" {
		ctx.ArchitectureChoice = imageArchitecture
	}

	return ctx
}

func readImageSource(ctx context.Context, sys *types.SystemContext, img string) (types.ImageSource, error) {
	ref, err := parseImageName(img)
	if err != nil {
		klog.Errorf("Could not parse image: %v", err)
		return nil, errors.Wrap(err, "Could not parse image")
	}

	src, err := ref.NewImageSource(ctx, sys)
	if err != nil {
		klog.Errorf("Could not create image reference: %v", err)
		return nil, NewImagePullFailedError(err)
	}
	return src, nil
}

func parseImageName(img string) (types.ImageReference, error) {
	parts := strings.SplitN(img, ":", 2)
	if len(parts) != 2 {
		return nil, errors.Errorf(`Invalid image name "%s", expected colon-separated transport:reference`, img)
	}
	switch parts[0] {
	case cdiv1.RegistrySchemeDocker:
		return docker.ParseReference(parts[1])
	case cdiv1.RegistrySchemeOci:
		return archive.ParseReference(parts[1])
	}
	return nil, errors.Errorf(`Invalid image name "%s", unknown transport`, img)
}

func closeImage(src types.ImageSource) {
	if err := src.Close(); err != nil {
		klog.Warningf("Could not close image source: %v ", err)
	}
}

func hasPrefix(path string, pathPrefix string) bool {
	return strings.HasPrefix(path, pathPrefix) ||
		strings.HasPrefix(path, "./"+pathPrefix)
}

func isWhiteout(path string) bool {
	return strings.HasPrefix(filepath.Base(path), whFilePrefix)
}

func isDir(hdr *tar.Header) bool {
	return hdr.Typeflag == tar.TypeDir
}

func processLayer(ctx context.Context,
	src types.ImageSource,
	layer types.BlobInfo,
	destDir string,
	pathPrefix string,
	cache types.BlobInfoCache,
	preallocation bool) (bool, error) {
	var reader io.ReadCloser
	reader, _, err := src.GetBlob(ctx, layer, cache)
	if err != nil {
		klog.Errorf("%v: %v", errReadingLayer, err)
		return false, fmt.Errorf("%w: %v", errReadingLayer, err)
	}
	fr, err := NewFormatReaders(reader, 0, nil)
	if err != nil {
		klog.Errorf("%v: %v", errReadingLayer, err)
		return false, fmt.Errorf("%w: %v", errReadingLayer, err)
	}
	defer fr.Close()

	tarReader := tar.NewReader(fr.TopReader())
	for {
		hdr, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break // End of archive
		}
		if err != nil {
			klog.Errorf("%v: %v", errReadingLayer, err)
			return false, fmt.Errorf("%w: %v", errReadingLayer, err)
		}

		if hasPrefix(hdr.Name, pathPrefix) && !isWhiteout(hdr.Name) && !isDir(hdr) {
			klog.Infof("File '%v' found in the layer", hdr.Name)
			destFile, err := safeJoinPaths(destDir, hdr.Name)
			if err != nil {
				klog.Errorf("Error sanitizing archive path: %v", err)
				return false, errors.Wrap(err, "Error sanitizing archive path")
			}

			if err = os.MkdirAll(filepath.Dir(destFile), os.ModePerm); err != nil {
				klog.Errorf("Error creating output file's directory: %v", err)
				return false, errors.Wrap(err, "Error creating output file's directory")
			}

			if _, _, err := StreamDataToFile(tarReader, destFile, preallocation); err != nil {
				klog.Errorf("Error copying file: %v", err)
				return false, errors.Wrap(err, "Error copying file")
			}

			return true, nil
		}
	}

	return false, nil
}

func streamRawLayer(ctx context.Context,
	src types.ImageSource,
	layer types.BlobInfo,
	destDir, pathPrefix string,
	cache types.BlobInfoCache,
	preallocation bool) error {
	var reader io.ReadCloser
	reader, _, err := src.GetBlob(ctx, layer, cache)
	if err != nil {
		klog.Errorf("%v: %v", errReadingLayer, err)
		return fmt.Errorf("%w: %v", errReadingLayer, err)
	}
	fr, err := NewFormatReaders(reader, 0, nil)
	if err != nil {
		klog.Errorf("%v: %v", errReadingLayer, err)
		return fmt.Errorf("%w: %v", errReadingLayer, err)
	}
	defer fr.Close()

	klog.Infof("Layer detected as raw disk image, streaming to disk")
	destFile := filepath.Join(destDir, pathPrefix, "disk.img")
	if err = os.MkdirAll(filepath.Dir(destFile), os.ModePerm); err != nil {
		return errors.Wrap(err, "error creating output directory")
	}
	if _, _, err := StreamDataToFile(fr.TopReader(), destFile, preallocation); err != nil {
		return errors.Wrap(err, "error streaming raw blob to file")
	}
	return nil
}

// Sanitize archive file pathing from "G305: Zip Slip vulnerability"
// https://security.snyk.io/research/zip-slip-vulnerability
func safeJoinPaths(dir, path string) (v string, err error) {
	v = filepath.Join(dir, path)
	wantPrefix := filepath.Clean(dir) + string(os.PathSeparator)

	if strings.HasPrefix(v, wantPrefix) {
		return v, nil
	}

	return "", fmt.Errorf("%s: %s", "content filepath is tainted", path)
}

// copyImage downloads the registry image and extracts the first disk image found
// under pathPrefix.
func (rd *RegistryDataSource) copyImage(destDir, pathPrefix string, preallocation bool) (*types.ImageInspectInfo, error) {
	klog.Infof("Downloading image from '%v', copying file from '%v' to '%v'", rd.endpoint, pathPrefix, destDir)

	ctx, cancel := commandTimeoutContext()
	defer cancel()
	srcCtx := buildSourceContext(rd.accessKey, rd.secKey, rd.imageArchitecture, rd.certDir, rd.insecureTLS)

	src, err := readImageSource(ctx, srcCtx, rd.endpoint)
	if err != nil {
		return nil, err
	}
	defer closeImage(src)

	cache := blobinfocache.DefaultCache(srcCtx)

	if len(rd.layerMatchAnnotations) > 0 {
		// OCI artifact layers hold the disk image, so there is no image to inspect afterwards.
		return nil, rd.copyArtifactLayer(ctx, srcCtx, src, cache, destDir, pathPrefix, preallocation)
	}

	imgCloser, err := image.FromSource(ctx, srcCtx, src)
	if err != nil {
		klog.Errorf("Error retrieving image: %v", err)
		return nil, errors.Wrap(err, "Error retrieving image")
	}
	defer imgCloser.Close()

	// in the event that target is not a manifest list / image index
	if srcCtx.ArchitectureChoice != "" {
		if err := validateImagePlatformMatch(srcCtx, imgCloser); err != nil {
			klog.Errorf("Error validating architecture: %v", err)
			return nil, fmt.Errorf("Error validating architecture: %w", err)
		}
	}

	if err := checkBootcImage(ctx, imgCloser); err != nil {
		return nil, err
	}

	found := false
	layers := imgCloser.LayerInfos()

	for _, layer := range layers {
		klog.Infof("Processing layer %+v", layer)

		found, err = processLayer(ctx, src, layer, destDir, pathPrefix, cache, preallocation)
		if found {
			break
		}
		if err != nil {
			if !errors.Is(err, errReadingLayer) {
				return nil, err
			}
			// Skipping layer and trying the next one.
			// Error already logged in processLayer
			continue
		}
	}

	if !found {
		klog.Errorf("Failed to find VM disk image file in the container image")
		return nil, errors.New("Failed to find VM disk image file in the container image")
	}

	info, err := imgCloser.Inspect(ctx)
	if err != nil {
		return nil, err
	}

	return info, nil
}

const (
	bootcImageLabel   = "containers.bootc"
	ostreeBootLabel   = "ostree.bootable"
	bootcLabelEnabled = "1"
)

func checkBootcImage(ctx context.Context, img types.Image) error {
	info, err := img.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("failed to inspect image for bootc detection: %w", err)
	}
	if info.Labels[bootcImageLabel] == bootcLabelEnabled ||
		info.Labels[ostreeBootLabel] == bootcLabelEnabled {
		klog.Infof("Detected bootc/ostree-bootable container image")
		return ErrBootcImageDetected
	}
	return nil
}

// copyArtifactLayer imports the single OCI artifact layer carrying the selected
// annotations. The layer blob holds the disk image itself, so it is streamed out as is
// instead of being walked for a disk image like a container image layer.
func (rd *RegistryDataSource) copyArtifactLayer(ctx context.Context, sys *types.SystemContext, src types.ImageSource, cache types.BlobInfoCache, destDir, pathPrefix string, preallocation bool) error {
	man, err := resolveArtifactManifest(ctx, sys, src, cache)
	if err != nil {
		return err
	}

	layer, err := findLayer(man, rd.layerMatchAnnotations)
	if err != nil {
		return err
	}

	klog.Infof("Layer %v selected by annotations %v", layer.Digest, rd.layerMatchAnnotations)
	return streamRawLayer(ctx, src, layer, destDir, pathPrefix, cache, preallocation)
}

// resolveArtifactManifest returns the OCI manifest of the artifact. An image index is
// resolved to the instance matching the platform requested in the system context, a plain
// manifest is verified against it.
func resolveArtifactManifest(ctx context.Context, sys *types.SystemContext, src types.ImageSource, cache types.BlobInfoCache) (*manifest.OCI1, error) {
	manBlob, mimeType, err := getManifest(ctx, src, nil)
	if err != nil {
		return nil, err
	}

	if manifest.MIMETypeIsMultiImage(mimeType) {
		list, err := manifest.ListFromBlob(manBlob, mimeType)
		if err != nil {
			return nil, errors.Wrap(err, "Error parsing image index")
		}
		instance, err := list.ChooseInstance(sys)
		if err != nil {
			return nil, errors.Wrap(err, "Error selecting a manifest from the image index")
		}
		if manBlob, mimeType, err = getManifest(ctx, src, &instance); err != nil {
			return nil, err
		}
		// The index entry of the chosen instance already declares the requested platform.
		return parseOCIManifest(manBlob, mimeType)
	}

	man, err := parseOCIManifest(manBlob, mimeType)
	if err != nil {
		return nil, err
	}
	if err := validateManifestPlatformMatch(ctx, src, cache, man, sys.ArchitectureChoice); err != nil {
		return nil, err
	}
	return man, nil
}

func parseOCIManifest(manBlob []byte, mimeType string) (*manifest.OCI1, error) {
	if mimeType != imgspecv1.MediaTypeImageManifest {
		return nil, errors.Errorf("Layer selection requires an OCI image manifest, got %q", mimeType)
	}
	return manifest.OCI1FromManifest(manBlob)
}

func getManifest(ctx context.Context, src types.ImageSource, instanceDigest *digest.Digest) ([]byte, string, error) {
	manBlob, mimeType, err := src.GetManifest(ctx, instanceDigest)
	if err != nil {
		return nil, "", errors.Wrap(err, "Error retrieving manifest")
	}
	if mimeType == "" {
		mimeType = manifest.GuessMIMEType(manBlob)
	}
	return manBlob, mimeType, nil
}

// validateManifestPlatformMatch fails when the manifest declares an architecture other than
// the requested one. A plain manifest has no platform of its own, so the architecture of its
// image configuration is all there is to go by, and an OCI artifact carrying a KubeVirt
// configuration instead of an image one declares no architecture at all.
func validateManifestPlatformMatch(ctx context.Context, src types.ImageSource, cache types.BlobInfoCache, man *manifest.OCI1, architecture string) error {
	if architecture == "" || man.Config.MediaType != imgspecv1.MediaTypeImageConfig {
		return nil
	}

	reader, _, err := src.GetBlob(ctx, manifest.BlobInfoFromOCI1Descriptor(man.Config), cache)
	if err != nil {
		return errors.Wrap(err, "Error reading image configuration")
	}
	defer reader.Close()

	var config imgspecv1.Image
	if err := json.NewDecoder(reader).Decode(&config); err != nil {
		return errors.Wrap(err, "Error parsing image configuration")
	}
	if config.Architecture != architecture {
		return errors.Errorf(`manifest image architecture: "%s" doesn't match requested architecture: "%s"`, config.Architecture, architecture)
	}
	return nil
}

// findLayer returns the single layer whose descriptor carries all of the given annotations.
// The selection has to be unambiguous, so several matching layers are an error just like none.
func findLayer(man *manifest.OCI1, matchAnnotations map[string]string) (types.BlobInfo, error) {
	var matched []imgspecv1.Descriptor
	for _, layer := range man.Layers {
		if matchesAnnotations(layer.Annotations, matchAnnotations) {
			matched = append(matched, layer)
		}
	}

	switch len(matched) {
	case 1:
		return manifest.BlobInfoFromOCI1Descriptor(matched[0]), nil
	case 0:
		return types.BlobInfo{}, errors.Errorf("No layer annotated with %v found in the manifest", matchAnnotations)
	default:
		digests := make([]string, 0, len(matched))
		for _, layer := range matched {
			digests = append(digests, layer.Digest.String())
		}
		return types.BlobInfo{}, errors.Errorf("Annotations %v match %d layers of the manifest (%s), they have to select a single one",
			matchAnnotations, len(matched), strings.Join(digests, ", "))
	}
}

func matchesAnnotations(annotations, matchAnnotations map[string]string) bool {
	for key, value := range matchAnnotations {
		if got, ok := annotations[key]; !ok || got != value {
			return false
		}
	}
	return true
}

func validateImagePlatformMatch(sys *types.SystemContext, img types.Image) error {
	config, err := img.OCIConfig(context.Background())
	if err != nil {
		return err
	}
	if config.Architecture != sys.ArchitectureChoice {
		return fmt.Errorf(`manifest image architecture: "%s" doesn't match requested architecture: "%s"`, config.Architecture, sys.ArchitectureChoice)
	}
	return nil
}

// GetImageDigest returns the digest of the container image at url.
// url: source registry url.
// accessKey: accessKey for the registry described in url.
// secKey: secretKey for the registry described in url.
// certDir: directory public CA keys are stored for registry identity verification
// insecureRegistry: boolean if true will allow insecure registries.
func GetImageDigest(url, accessKey, secKey, certDir string, insecureRegistry bool) (string, error) {
	klog.Infof("Inspecting image from '%v'", url)

	ctx, cancel := commandTimeoutContext()
	defer cancel()
	srcCtx := buildSourceContext(accessKey, secKey, "", certDir, insecureRegistry)

	src, err := readImageSource(ctx, srcCtx, url)
	if err != nil {
		return "", err
	}
	defer closeImage(src)

	imageManifest, _, err := src.GetManifest(context.Background(), nil)
	if err != nil {
		return "", err
	}

	digest, err := manifest.Digest(imageManifest)
	if err != nil {
		return "", err
	}

	return digest.String(), nil
}
