package bootc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitlab.com/bootc-org/podman-bootc/pkg/config"
	"gitlab.com/bootc-org/podman-bootc/pkg/user"
	"gitlab.com/bootc-org/podman-bootc/pkg/utils"

	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/images"
	"github.com/containers/podman/v5/pkg/domain/entities/types"
	"github.com/containers/podman/v5/pkg/specgen"
	"github.com/docker/go-units"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// As a baseline heuristic we double the size of
// the input container to support in-place updates.
// This is planned to be more configurable in the
// future.  See also bootc-image-builder
const containerSizeToDiskSizeMultiplier = 2
const diskSizeMinimum = 10 * 1024 * 1024 * 1024 // 10GB
const imageMetaXattr = "user.bootc.meta"

// tempLosetupWrapperContents is a workaround for https://github.com/containers/bootc/pull/487/commits/89d34c7dbcb8a1fa161f812c6ba0a8b49ccbe00f
const tempLosetupWrapperContents = `#!/bin/bash
set -euo pipefail
args=(/usr/sbin/losetup --direct-io=off)
for arg in "$@"; do
	case $arg in
		--direct-io=*) echo "ignoring: $arg" 1>&2;;
		*) args+=("$arg") ;;
	esac
done
exec "${args[@]}"
`

// DiskImageConfig defines configuration for the
type DiskImageConfig struct {
	Filesystem  string
	RootSizeMax string
	DiskSize    string
}

// diskFromContainerMeta is serialized to JSON in a user xattr on a disk image
type diskFromContainerMeta struct {
	// imageDigest is the digested sha256 of the container that was used to build this disk
	ImageDigest string `json:"imageDigest"`
}

type BootcDisk struct {
	ImageNameOrId           string
	User                    user.User
	Ctx                     context.Context
	ImageId                 string
	imageData               *types.ImageInspectReport
	RepoTag                 string
	CreatedAt               time.Time
	Directory               string
	file                    *os.File
	bootcInstallContainerId string
}

// create singleton for easy cleanup
var (
	instance     *BootcDisk
	instanceOnce sync.Once
)

func NewBootcDisk(imageNameOrId string, ctx context.Context, user user.User) *BootcDisk {
	instanceOnce.Do(func() {
		instance = &BootcDisk{
			ImageNameOrId: imageNameOrId,
			Ctx:           ctx,
			User:          user,
		}
	})
	return instance
}

func (p *BootcDisk) GetDirectory() string {
	return p.Directory
}

func (p *BootcDisk) GetImageId() string {
	return p.ImageId
}

// GetSize returns the virtual size of the disk in bytes;
// this may be larger than the actual disk usage
func (p *BootcDisk) GetSize() (int64, error) {
	st, err := os.Stat(filepath.Join(p.Directory, config.DiskImage))
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// GetRepoTag returns the repository of the container image
func (p *BootcDisk) GetRepoTag() string {
	return p.RepoTag
}

// GetCreatedAt returns the creation time of the disk image
func (p *BootcDisk) GetCreatedAt() time.Time {
	return p.CreatedAt
}

func (p *BootcDisk) Install(quiet bool, config DiskImageConfig) (err error) {
	p.CreatedAt = time.Now()

	err = p.pullImage()
	if err != nil {
		return
	}

	// Create VM cache dir; one per oci bootc image
	p.Directory = filepath.Join(p.User.CacheDir(), p.ImageId)
	lock := utils.NewCacheLock(p.User.RunDir(), p.Directory)
	locked, err := lock.TryLock(utils.Exclusive)
	if err != nil {
		return fmt.Errorf("error locking the VM cache path: %w", err)
	}
	if !locked {
		return fmt.Errorf("unable to lock the VM cache path")
	}

	defer func() {
		if err := lock.Unlock(); err != nil {
			logrus.Errorf("unable to unlock VM %s: %v", p.ImageId, err)
		}
	}()

	if err := os.MkdirAll(p.Directory, os.ModePerm); err != nil {
		return fmt.Errorf("error while making bootc disk directory: %w", err)
	}

	err = p.getOrInstallImageToDisk(quiet, config)
	if err != nil {
		return
	}

	elapsed := time.Since(p.CreatedAt)
	logrus.Debugf("installImage elapsed: %v", elapsed)

	return
}

func (p *BootcDisk) Cleanup() (err error) {
	force := true
	if p.bootcInstallContainerId != "" {
		_, err := containers.Remove(p.Ctx, p.bootcInstallContainerId, &containers.RemoveOptions{Force: &force})
		if err != nil {
			return fmt.Errorf("failed to remove bootc install container: %w", err)
		}
	}

	return
}

// getOrInstallImageToDisk checks if the disk is present and if not, installs the image to a new disk
func (p *BootcDisk) getOrInstallImageToDisk(quiet bool, diskConfig DiskImageConfig) error {
	diskPath := filepath.Join(p.Directory, config.DiskImage)
	f, err := os.Open(diskPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		logrus.Debugf("No existing disk image found")
		return p.bootcInstallImageToDisk(quiet, diskConfig)
	}
	logrus.Debug("Found existing disk image, comparing digest")
	defer f.Close()
	buf := make([]byte, 4096)
	len, err := unix.Fgetxattr(int(f.Fd()), imageMetaXattr, buf)
	if err != nil {
		// If there's no xattr, just remove it
		os.Remove(diskPath)
		logrus.Debugf("No %s xattr found", imageMetaXattr)
		return p.bootcInstallImageToDisk(quiet, diskConfig)
	}
	bufTrimmed := buf[:len]
	var serializedMeta diskFromContainerMeta
	if err := json.Unmarshal(bufTrimmed, &serializedMeta); err != nil {
		logrus.Warnf("failed to parse serialized meta from %s (%v) %v", diskPath, buf, err)
		return p.bootcInstallImageToDisk(quiet, diskConfig)
	}

	logrus.Debugf("previous disk digest: %s current digest: %s", serializedMeta.ImageDigest, p.ImageId)
	if serializedMeta.ImageDigest == p.ImageId {
		return nil
	}

	return p.bootcInstallImageToDisk(quiet, diskConfig)
}

func align(size int64, align int64) int64 {
	rem := size % align
	if rem != 0 {
		size += (align - rem)
	}
	return size
}

// bootcInstallImageToDisk creates a disk image from a bootc container
func (p *BootcDisk) bootcInstallImageToDisk(quiet bool, diskConfig DiskImageConfig) (err error) {
	fmt.Printf("Executing `bootc install to-disk` from container image %s to create disk image\n", p.RepoTag)
	p.file, err = os.CreateTemp(p.Directory, "podman-bootc-tempdisk")
	if err != nil {
		return err
	}
	size := p.imageData.Size * containerSizeToDiskSizeMultiplier
	if size < diskSizeMinimum {
		size = diskSizeMinimum
	}
	if diskConfig.DiskSize != "" {
		diskConfigSize, err := units.FromHumanSize(diskConfig.DiskSize)
		if err != nil {
			return err
		}
		if size < diskConfigSize {
			size = diskConfigSize
		}
	}
	// Round up to 4k; loopback wants at least 512b alignment
	size = align(size, 4096)
	humanContainerSize := units.HumanSize(float64(p.imageData.Size))
	humanSize := units.HumanSize(float64(size))
	logrus.Infof("container size: %s, disk size: %s", humanContainerSize, humanSize)

	if err := syscall.Ftruncate(int(p.file.Fd()), size); err != nil {
		return err
	}
	logrus.Debugf("Created %s with size %v", p.file.Name(), size)
	doCleanupDisk := true
	defer func() {
		if doCleanupDisk {
			os.Remove(p.file.Name())
		}
	}()

	err = p.runInstallContainer(quiet, diskConfig)
	if err != nil {
		return fmt.Errorf("failed to create disk image: %w", err)
	}
	serializedMeta := diskFromContainerMeta{
		ImageDigest: p.ImageId,
	}
	buf, err := json.Marshal(serializedMeta)
	if err != nil {
		return err
	}
	if err := unix.Fsetxattr(int(p.file.Fd()), imageMetaXattr, buf, 0); err != nil {
		return fmt.Errorf("failed to set xattr: %w", err)
	}
	diskPath := filepath.Join(p.Directory, config.DiskImage)

	if err := os.Rename(p.file.Name(), diskPath); err != nil {
		return fmt.Errorf("failed to rename to %s: %w", diskPath, err)
	}
	doCleanupDisk = false

	return nil
}

// pullImage fetches the container image if not present
func (p *BootcDisk) pullImage() (err error) {
	pullPolicy := "missing"
	ids, err := images.Pull(p.Ctx, p.ImageNameOrId, &images.PullOptions{Policy: &pullPolicy})
	if err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	if len(ids) == 0 {
		return fmt.Errorf("no ids returned from image pull")
	}

	if len(ids) > 1 {
		return fmt.Errorf("multiple ids returned from image pull")
	}

	image, err := images.GetImage(p.Ctx, p.ImageNameOrId, &images.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get image: %w", err)
	}
	p.imageData = image

	imageId := ids[0]
	p.ImageId = imageId
	p.RepoTag = image.RepoTags[0]

	return
}

// runInstallContainer runs the bootc installer in a container to create a disk image
func (p *BootcDisk) runInstallContainer(quiet bool, config DiskImageConfig) (err error) {
	// Create a temporary external shell script with the contents of our losetup wrapper
	losetupTemp, err := os.CreateTemp(p.Directory, "losetup-wrapper")
	if err != nil {
		return fmt.Errorf("temp losetup wrapper: %w", err)
	}
	defer os.Remove(losetupTemp.Name())
	if _, err := io.Copy(losetupTemp, strings.NewReader(tempLosetupWrapperContents)); err != nil {
		return fmt.Errorf("temp losetup wrapper copy: %w", err)
	}
	if err := losetupTemp.Chmod(0o755); err != nil {
		return fmt.Errorf("temp losetup wrapper chmod: %w", err)
	}

	createResponse, err := p.createInstallContainer(config, losetupTemp.Name())
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	p.bootcInstallContainerId = createResponse.ID //save the id for possible cleanup
	logrus.Debugf("Created install container, id=%s", createResponse.ID)

	// run the container to create the disk
	err = containers.Start(p.Ctx, p.bootcInstallContainerId, &containers.StartOptions{})
	if err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}
	logrus.Debugf("Started install container")

	// Ensure we've cancelled the container attachment when exiting this function, as
	// it takes over stdout/stderr handling
	attachCancelCtx, cancelAttach := context.WithCancel(p.Ctx)
	defer cancelAttach()
	var exitCode int32
	if !quiet {
		attachOpts := new(containers.AttachOptions).WithStream(true)
		if err := containers.Attach(attachCancelCtx, p.bootcInstallContainerId, nil, os.Stdout, os.Stderr, nil, attachOpts); err != nil {
			return fmt.Errorf("attaching: %w", err)
		}
	}
	exitCode, err = containers.Wait(p.Ctx, p.bootcInstallContainerId, nil)
	if err != nil {
		return fmt.Errorf("failed to wait for container: %w", err)
	}

	if exitCode != 0 {
		return fmt.Errorf("failed to run bootc install")
	}

	return
}

// createInstallContainer creates a container to run the bootc installer
func (p *BootcDisk) createInstallContainer(config DiskImageConfig, tempLosetup string) (createResponse types.ContainerCreateResponse, err error) {
	privileged := true
	autoRemove := true
	labelNested := true

	targetEnv := make(map[string]string)
	if v, ok := os.LookupEnv("BOOTC_INSTALL_LOG"); ok {
		targetEnv["RUST_LOG"] = v
	}

	bootcInstallArgs := []string{
		"bootc", "install", "to-disk", "--via-loopback", "--generic-image",
		"--skip-fetch-check",
	}
	if config.Filesystem != "" {
		bootcInstallArgs = append(bootcInstallArgs, "--filesystem", config.Filesystem)
	}
	if config.RootSizeMax != "" {
		bootcInstallArgs = append(bootcInstallArgs, "--root-size="+config.RootSizeMax)
	}
	bootcInstallArgs = append(bootcInstallArgs, "/output/"+filepath.Base(p.file.Name()))

	// Allocate pty so we can show progress bars, spinners etc.
	trueDat := true
	s := &specgen.SpecGenerator{
		ContainerBasicConfig: specgen.ContainerBasicConfig{
			Command:     bootcInstallArgs,
			PidNS:       specgen.Namespace{NSMode: specgen.Host},
			Remove:      &autoRemove,
			Annotations: map[string]string{"io.podman.annotations.label": "type:unconfined_t"},
			Env:         targetEnv,
			Terminal:    &trueDat,
		},
		ContainerStorageConfig: specgen.ContainerStorageConfig{
			Image: p.ImageNameOrId,
			Mounts: []specs.Mount{
				{
					Source:      "/var/lib/containers",
					Destination: "/var/lib/containers",
					Type:        "bind",
				},
				{
					Source:      "/dev",
					Destination: "/dev",
					Type:        "bind",
				},
				{
					Source:      p.Directory,
					Destination: "/output",
					Type:        "bind",
				},
				{
					Source: tempLosetup,
					// Note that the default $PATH has /usr/local/sbin first
					Destination: "/usr/local/sbin/losetup",
					Type:        "bind",
					Options:     []string{"ro"},
				},
			},
		},
		ContainerSecurityConfig: specgen.ContainerSecurityConfig{
			Privileged:  &privileged,
			LabelNested: &labelNested,
			SelinuxOpts: []string{"type:unconfined_t"},
		},
		ContainerNetworkConfig: specgen.ContainerNetworkConfig{
			NetNS: specgen.Namespace{
				NSMode: specgen.Bridge,
			},
		},
	}

	createResponse, err = containers.CreateWithSpec(p.Ctx, s, &containers.CreateOptions{})
	if err != nil {
		return createResponse, fmt.Errorf("failed to create container: %w", err)
	}

	return
}
