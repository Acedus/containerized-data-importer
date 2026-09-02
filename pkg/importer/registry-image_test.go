package importer

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/containers/image/v5/types"
)

var _ = Describe("Registry image", func() {
	source := "oci-archive:" + imageFile
	malformedSource := "oci-archive:" + filepath.Join(imageDir, "malformed-registry-image.tar")
	multiArchSource := "oci-archive:" + filepath.Join(imageDir, "multiarch-registry-image.tar")
	bootcSource := "oci-archive:" + filepath.Join(imageDir, "bootc-registry-image.tar")
	const diskImage = containerDiskImageDir + "/cirros-0.3.4-x86_64-disk.img"

	var tmpDir string

	// copyFile pulls source and extracts the first file under pathPrefix into tmpDir.
	copyFile := func(source, architecture, pathPrefix string) (*types.ImageInspectInfo, error) {
		image := newRegistryImage(source, withArchitecture(architecture))
		return image.copyFile(context.Background(), fileExtractor{destDir: tmpDir, pathPrefix: pathPrefix})
	}

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "scratch")
		Expect(err).NotTo(HaveOccurred())
		By("tmpDir: " + tmpDir)
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	DescribeTable("Should extract a single file", func(source string) {
		info, err := copyFile(source, "", diskImage)
		Expect(err).ToNot(HaveOccurred())
		Expect(info).ToNot(BeNil())
		Expect(filepath.Join(tmpDir, diskImage)).To(BeARegularFile())
	},
		Entry("when all image layers are valid", source),
		Entry("when one of the image layers is malformed", malformedSource),
	)

	It("Should return an error if a single file is not found", func() {
		info, err := copyFile(source, "", containerDiskImageDir+"/invalid.img")
		Expect(err).To(HaveOccurred())
		Expect(info).To(BeNil())
		Expect(filepath.Join(tmpDir, diskImage)).ToNot(BeAnExistingFile())
	})

	It("Should return an error on an endpoint with an unknown transport", func() {
		info, err := copyFile("invalid", "", containerDiskImageDir)
		Expect(err).To(MatchError(ContainSubstring("Could not parse image")))
		Expect(info).To(BeNil())
	})

	DescribeTable("Should correctly assert image architecture", func(source string, architecture string, wantErr bool) {
		info, err := copyFile(source, architecture, containerDiskImageDir)
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
		info, err := copyFile(bootcSource, "", containerDiskImageDir)
		Expect(err).To(MatchError(ErrBootcImageDetected))
		Expect(info).To(BeNil())
	})

	It("Should not detect a non-bootc image as bootc", func() {
		info, err := copyFile(source, "", diskImage)
		Expect(err).ToNot(HaveOccurred())
		Expect(info).ToNot(BeNil())
	})
})
