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
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/opencontainers/go-digest"
)

var _ = Describe("Registry Importer", func() {
	source := "oci-archive:" + imageFile
	malformedSource := "oci-archive:" + filepath.Join(imageDir, "malformed-registry-image.tar")
	multiArchSource := "oci-archive:" + filepath.Join(imageDir, "multiarch-registry-image.tar")
	bootcSource := "oci-archive:" + filepath.Join(imageDir, "bootc-registry-image.tar")
	// KubeVirt VM OCI artifact: an image index with an amd64 manifest holding a rootdisk and a
	// datadisk raw+zstd layer, and an arm64 manifest holding a rootdisk layer. Every layer
	// carries the same io.kubevirt.disk.size, so that annotation alone selects several.
	kubevirtOCISource := "oci-archive:" + filepath.Join(imageDir, "kubevirt-vm-oci-image.tar")
	const (
		diskNameAnnotation    = "io.kubevirt.disk.name"
		diskSizeAnnotation    = "io.kubevirt.disk.size"
		amd64RootDiskChecksum = "sha256:f670e4e408beaec3329c14a99ebee6a3fd23ecbde36386ed0da99b51218e66ef"
		amd64DataDiskChecksum = "sha256:aa8b53f1d73aa7fe15b0cb0a07bbf97e3eae4e216bd4cccdee3446f0742fe8c4"
		arm64RootDiskChecksum = "sha256:d444acc912c6ada37afb62f63d079e0cc95f4d32b95be232a897d8cd42d1c2d5"
	)
	layerOf := func(value string) map[string]string {
		return map[string]string{diskNameAnnotation: value}
	}

	var tmpDir string
	var err error

	BeforeEach(func() {
		tmpDir, err = os.MkdirTemp("", "scratch")
		Expect(err).NotTo(HaveOccurred())
		By("tmpDir: " + tmpDir)
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	DescribeTable("Should extract a single file", func(source string) {
		info, err := (&RegistryDataSource{endpoint: source}).copyImage(tmpDir, "disk/cirros-0.3.4-x86_64-disk.img", false)
		Expect(err).ToNot(HaveOccurred())
		Expect(info).ToNot(BeNil())

		file := filepath.Join(tmpDir, "disk/cirros-0.3.4-x86_64-disk.img")
		Expect(file).To(BeARegularFile())
	},
		Entry("when all image layers are valid", source),
		Entry("when one of the image layers is malformed", malformedSource),
	)
	It("Should return an error if a single file is not found", func() {
		info, err := (&RegistryDataSource{endpoint: source}).copyImage(tmpDir, "disk/invalid.img", false)
		Expect(err).To(HaveOccurred())
		Expect(info).To(BeNil())

		file := filepath.Join(tmpDir, "disk/cirros-0.3.4-x86_64-disk.img")
		_, err = os.Stat(file)
		Expect(err).To(HaveOccurred())
	})
	DescribeTable("Should extract the disk image of the layer selected by annotation", func(architecture string, matchAnnotations map[string]string, checksum string) {
		rd := &RegistryDataSource{endpoint: kubevirtOCISource, imageArchitecture: architecture, layerMatchAnnotations: matchAnnotations}
		info, err := rd.copyImage(tmpDir, "disk", false)
		Expect(err).ToNot(HaveOccurred())
		// an OCI artifact layer is the disk image, there is no image to inspect
		Expect(info).To(BeNil())

		data, err := os.ReadFile(filepath.Join(tmpDir, "disk", "disk.img"))
		Expect(err).ToNot(HaveOccurred())
		Expect(data).To(HaveLen(65536))
		Expect(digest.FromBytes(data)).To(Equal(digest.Digest(checksum)))
	},
		Entry("first layer of the manifest selected by architecture", "amd64", layerOf("rootdisk"), amd64RootDiskChecksum),
		Entry("later layer of the manifest", "amd64", layerOf("datadisk"), amd64DataDiskChecksum),
		Entry("layer of another architecture manifest", "arm64", layerOf("rootdisk"), arm64RootDiskChecksum),
		Entry("layer carrying every annotation given", "amd64",
			map[string]string{diskNameAnnotation: "rootdisk", diskSizeAnnotation: "64Ki"}, amd64RootDiskChecksum),
	)

	DescribeTable("Should return an error if no layer is selected", func(architecture string, matchAnnotations map[string]string) {
		rd := &RegistryDataSource{endpoint: kubevirtOCISource, imageArchitecture: architecture, layerMatchAnnotations: matchAnnotations}
		info, err := rd.copyImage(tmpDir, "disk", false)
		Expect(err).To(HaveOccurred())
		Expect(info).To(BeNil())
		Expect(filepath.Join(tmpDir, "disk", "disk.img")).ToNot(BeAnExistingFile())
	},
		Entry("when no layer carries the annotation", "amd64", map[string]string{"io.kubevirt.disk.unknown": "rootdisk"}),
		Entry("when no layer annotation has the value", "amd64", layerOf("unknowndisk")),
		Entry("when only some of the annotations match", "amd64",
			map[string]string{diskNameAnnotation: "rootdisk", diskSizeAnnotation: "10Gi"}),
		Entry("when the annotated layer belongs to another architecture", "arm64", layerOf("datadisk")),
		Entry("when the image index has no manifest for the architecture", "invalid", layerOf("rootdisk")),
	)

	It("Should return an error if the annotations select more than one layer", func() {
		rd := &RegistryDataSource{endpoint: kubevirtOCISource, imageArchitecture: "amd64",
			layerMatchAnnotations: map[string]string{diskSizeAnnotation: "64Ki"}}
		info, err := rd.copyImage(tmpDir, "disk", false)
		Expect(err).To(MatchError(ContainSubstring("match 2 layers of the manifest")))
		Expect(info).To(BeNil())
		Expect(filepath.Join(tmpDir, "disk", "disk.img")).ToNot(BeAnExistingFile())
	})

	DescribeTable("Should correctly assert image architecture", func(source string, architecture string, wantErr bool) {
		info, err := (&RegistryDataSource{endpoint: source, imageArchitecture: architecture}).copyImage(tmpDir, "disk/", false)
		if wantErr {
			Expect(err).To(HaveOccurred())
			Expect(info).To(BeNil())
		} else {
			Expect(err).ToNot(HaveOccurred())
			Expect(info).ToNot(BeNil())
		}
	},
		Entry("when archive is a image and architecture doesn't match specified architecture", source, "arm64", true),
		Entry("when archive is a image and architecture matches specified arechitecture", source, "amd64", false),
		Entry("when archive is an image index and architecture doesn't match specified architecture", multiArchSource, "invalid", true),
		Entry("when archive is an image index and architecture matches specified architecture", multiArchSource, "amd64", false),
	)

	It("Should detect a bootc image and return ErrBootcImageDetected", func() {
		info, err := (&RegistryDataSource{endpoint: bootcSource}).copyImage(tmpDir, "disk/", false)
		Expect(err).To(HaveOccurred())
		Expect(err).To(MatchError(ErrBootcImageDetected))
		Expect(info).To(BeNil())
	})

	It("Should not detect a non-bootc image as bootc", func() {
		info, err := (&RegistryDataSource{endpoint: source}).copyImage(tmpDir, "disk/cirros-0.3.4-x86_64-disk.img", false)
		Expect(err).ToNot(HaveOccurred())
		Expect(info).ToNot(BeNil())
	})
})
