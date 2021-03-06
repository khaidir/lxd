package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/gorilla/websocket"

	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/idmap"
	"github.com/lxc/lxd/shared/logger"

	"github.com/pborman/uuid"
)

var zfsUseRefquota = "false"
var zfsRemoveSnapshots = "false"

type storageZfs struct {
	dataset string
	storageShared
}

func (s *storageZfs) getOnDiskPoolName() string {
	if s.dataset != "" {
		return s.dataset
	}

	return s.pool.Name
}

// Only initialize the minimal information we need about a given storage type.
func (s *storageZfs) StorageCoreInit() error {
	s.sType = storageTypeZfs
	typeName, err := storageTypeToString(s.sType)
	if err != nil {
		return err
	}
	s.sTypeName = typeName

	util.LoadModule("zfs")

	if !zfsIsEnabled() {
		return fmt.Errorf("the \"zfs\" tool is not enabled")
	}

	s.sTypeVersion, err = zfsModuleVersionGet()
	if err != nil {
		return err
	}

	logger.Debugf("Initializing a ZFS driver.")
	return nil
}

// Functions dealing with storage pools.
func (s *storageZfs) StoragePoolInit() error {
	err := s.StorageCoreInit()
	if err != nil {
		return err
	}

	// Detect whether we have been given a zfs dataset as source.
	if s.pool.Config["zfs.pool_name"] != "" {
		s.dataset = s.pool.Config["zfs.pool_name"]
	}

	return nil
}

func (s *storageZfs) StoragePoolCheck() error {
	logger.Debugf("Checking ZFS storage pool \"%s\".", s.pool.Name)

	source := s.pool.Config["source"]
	if source == "" {
		return fmt.Errorf("no \"source\" property found for the storage pool")
	}

	poolName := s.getOnDiskPoolName()
	if filepath.IsAbs(source) {
		if zfsFilesystemEntityExists(poolName, "") {
			return nil
		}
		logger.Debugf("ZFS storage pool \"%s\" does not exist. Trying to import it.", poolName)

		disksPath := shared.VarPath("disks")
		output, err := shared.RunCommand(
			"zpool",
			"import",
			"-d", disksPath, poolName)
		if err != nil {
			return fmt.Errorf("ZFS storage pool \"%s\" could not be imported: %s", poolName, output)
		}

		logger.Debugf("ZFS storage pool \"%s\" successfully imported.", poolName)
	}

	return nil
}

func (s *storageZfs) StoragePoolCreate() error {
	logger.Infof("Creating ZFS storage pool \"%s\".", s.pool.Name)

	err := s.zfsPoolCreate()
	if err != nil {
		return err
	}
	revert := true
	defer func() {
		if !revert {
			return
		}
		s.StoragePoolDelete()
	}()

	storagePoolMntPoint := getStoragePoolMountPoint(s.pool.Name)
	err = os.MkdirAll(storagePoolMntPoint, 0755)
	if err != nil {
		return err
	}

	err = s.StoragePoolCheck()
	if err != nil {
		return err
	}

	revert = false

	logger.Infof("Created ZFS storage pool \"%s\".", s.pool.Name)
	return nil
}

func (s *storageZfs) zfsPoolCreate() error {
	s.pool.Config["volatile.initial_source"] = s.pool.Config["source"]

	zpoolName := s.getOnDiskPoolName()
	vdev := s.pool.Config["source"]
	if vdev == "" {
		vdev = filepath.Join(shared.VarPath("disks"), fmt.Sprintf("%s.img", s.pool.Name))
		s.pool.Config["source"] = vdev

		if s.pool.Config["zfs.pool_name"] == "" {
			s.pool.Config["zfs.pool_name"] = zpoolName
		}

		f, err := os.Create(vdev)
		if err != nil {
			return fmt.Errorf("Failed to open %s: %s", vdev, err)
		}
		defer f.Close()

		err = f.Chmod(0600)
		if err != nil {
			return fmt.Errorf("Failed to chmod %s: %s", vdev, err)
		}

		size, err := shared.ParseByteSizeString(s.pool.Config["size"])
		if err != nil {
			return err
		}
		err = f.Truncate(size)
		if err != nil {
			return fmt.Errorf("Failed to create sparse file %s: %s", vdev, err)
		}

		err = zfsPoolCreate(zpoolName, vdev)
		if err != nil {
			return err
		}
	} else {
		// Unset size property since it doesn't make sense.
		s.pool.Config["size"] = ""

		if filepath.IsAbs(vdev) {
			if !shared.IsBlockdevPath(vdev) {
				return fmt.Errorf("custom loop file locations are not supported")
			}

			if s.pool.Config["zfs.pool_name"] == "" {
				s.pool.Config["zfs.pool_name"] = zpoolName
			}

			// This is a block device. Note, that we do not store the
			// block device path or UUID or PARTUUID or similar in
			// the database. All of those might change or might be
			// used in a special way (For example, zfs uses a single
			// UUID in a multi-device pool for all devices.). The
			// safest way is to just store the name of the zfs pool
			// we create.
			s.pool.Config["source"] = zpoolName
			err := zfsPoolCreate(zpoolName, vdev)
			if err != nil {
				return err
			}
		} else {
			if s.pool.Config["zfs.pool_name"] != "" {
				return fmt.Errorf("invalid combination of \"source\" and \"zfs.pool_name\" property")
			}
			s.pool.Config["zfs.pool_name"] = vdev
			s.dataset = vdev

			if strings.Contains(vdev, "/") {
				if !zfsFilesystemEntityExists(vdev, "") {
					err := zfsPoolCreate("", vdev)
					if err != nil {
						return err
					}
				}
			} else {
				err := zfsPoolCheck(vdev)
				if err != nil {
					return err
				}
			}

			subvols, err := zfsPoolListSubvolumes(zpoolName, vdev)
			if err != nil {
				return err
			}

			if len(subvols) > 0 {
				return fmt.Errorf("Provided ZFS pool (or dataset) isn't empty")
			}

			if err := zfsPoolVolumeSet(vdev, "", "mountpoint", "none"); err != nil {
				return err
			}
		}
	}

	// Create default dummy datasets to avoid zfs races during container
	// creation.
	poolName := s.getOnDiskPoolName()
	dataset := fmt.Sprintf("%s/containers", poolName)
	msg, err := zfsPoolVolumeCreate(dataset, "mountpoint=none")
	if err != nil {
		logger.Errorf("failed to create containers dataset: %s", msg)
		return err
	}

	fixperms := shared.VarPath("storage-pools", s.pool.Name, "containers")
	err = os.MkdirAll(fixperms, containersDirMode)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	err = os.Chmod(fixperms, containersDirMode)
	if err != nil {
		logger.Warnf("failed to chmod \"%s\" to \"0%s\": %s", fixperms, strconv.FormatInt(int64(containersDirMode), 8), err)
	}

	dataset = fmt.Sprintf("%s/images", poolName)
	msg, err = zfsPoolVolumeCreate(dataset, "mountpoint=none")
	if err != nil {
		logger.Errorf("failed to create images dataset: %s", msg)
		return err
	}

	fixperms = shared.VarPath("storage-pools", s.pool.Name, "images")
	err = os.MkdirAll(fixperms, imagesDirMode)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	err = os.Chmod(fixperms, imagesDirMode)
	if err != nil {
		logger.Warnf("failed to chmod \"%s\" to \"0%s\": %s", fixperms, strconv.FormatInt(int64(imagesDirMode), 8), err)
	}

	dataset = fmt.Sprintf("%s/custom", poolName)
	msg, err = zfsPoolVolumeCreate(dataset, "mountpoint=none")
	if err != nil {
		logger.Errorf("failed to create custom dataset: %s", msg)
		return err
	}

	fixperms = shared.VarPath("storage-pools", s.pool.Name, "custom")
	err = os.MkdirAll(fixperms, customDirMode)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	err = os.Chmod(fixperms, customDirMode)
	if err != nil {
		logger.Warnf("failed to chmod \"%s\" to \"0%s\": %s", fixperms, strconv.FormatInt(int64(customDirMode), 8), err)
	}

	dataset = fmt.Sprintf("%s/deleted", poolName)
	msg, err = zfsPoolVolumeCreate(dataset, "mountpoint=none")
	if err != nil {
		logger.Errorf("failed to create deleted dataset: %s", msg)
		return err
	}

	dataset = fmt.Sprintf("%s/snapshots", poolName)
	msg, err = zfsPoolVolumeCreate(dataset, "mountpoint=none")
	if err != nil {
		logger.Errorf("failed to create snapshots dataset: %s", msg)
		return err
	}

	fixperms = shared.VarPath("storage-pools", s.pool.Name, "snapshots")
	err = os.MkdirAll(fixperms, snapshotsDirMode)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	err = os.Chmod(fixperms, snapshotsDirMode)
	if err != nil {
		logger.Warnf("failed to chmod \"%s\" to \"0%s\": %s", fixperms, strconv.FormatInt(int64(snapshotsDirMode), 8), err)
	}

	return nil
}

func (s *storageZfs) StoragePoolDelete() error {
	logger.Infof("Deleting ZFS storage pool \"%s\".", s.pool.Name)

	poolName := s.getOnDiskPoolName()
	if zfsFilesystemEntityExists(poolName, "") {
		err := zfsFilesystemEntityDelete(s.pool.Config["source"], poolName)
		if err != nil {
			return err
		}
	}

	storagePoolMntPoint := getStoragePoolMountPoint(s.pool.Name)
	if shared.PathExists(storagePoolMntPoint) {
		err := os.RemoveAll(storagePoolMntPoint)
		if err != nil {
			return err
		}
	}

	logger.Infof("Deleted ZFS storage pool \"%s\".", s.pool.Name)
	return nil
}

func (s *storageZfs) StoragePoolMount() (bool, error) {
	return true, nil
}

func (s *storageZfs) StoragePoolUmount() (bool, error) {
	return true, nil
}

func (s *storageZfs) StoragePoolVolumeCreate() error {
	logger.Infof("Creating ZFS storage volume \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)

	fs := fmt.Sprintf("custom/%s", s.volume.Name)
	poolName := s.getOnDiskPoolName()
	dataset := fmt.Sprintf("%s/%s", poolName, fs)
	customPoolVolumeMntPoint := getStoragePoolVolumeMountPoint(s.pool.Name, s.volume.Name)

	msg, err := zfsPoolVolumeCreate(dataset, "mountpoint=none", "canmount=noauto")
	if err != nil {
		logger.Errorf("failed to create ZFS storage volume \"%s\" on storage pool \"%s\": %s", s.volume.Name, s.pool.Name, msg)
		return err
	}
	revert := true
	defer func() {
		if !revert {
			return
		}
		s.StoragePoolVolumeDelete()
	}()

	err = zfsPoolVolumeSet(poolName, fs, "mountpoint", customPoolVolumeMntPoint)
	if err != nil {
		return err
	}

	if !shared.IsMountPoint(customPoolVolumeMntPoint) {
		zfsMount(poolName, fs)
	}

	// apply quota
	if s.volume.Config["size"] != "" {
		size, err := shared.ParseByteSizeString(s.volume.Config["size"])
		if err != nil {
			return err
		}

		err = s.StorageEntitySetQuota(storagePoolVolumeTypeCustom, size, nil)
		if err != nil {
			return err
		}
	}

	revert = false

	logger.Infof("Created ZFS storage volume \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)
	return nil
}

func (s *storageZfs) StoragePoolVolumeDelete() error {
	logger.Infof("Deleting ZFS storage volume \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)

	fs := fmt.Sprintf("custom/%s", s.volume.Name)
	customPoolVolumeMntPoint := getStoragePoolVolumeMountPoint(s.pool.Name, s.volume.Name)

	poolName := s.getOnDiskPoolName()
	if zfsFilesystemEntityExists(poolName, fs) {
		err := zfsPoolVolumeDestroy(s.getOnDiskPoolName(), fs)
		if err != nil {
			return err
		}
	}

	if shared.PathExists(customPoolVolumeMntPoint) {
		err := os.RemoveAll(customPoolVolumeMntPoint)
		if err != nil {
			return err
		}
	}

	err := db.StoragePoolVolumeDelete(
		s.s.DB,
		s.volume.Name,
		storagePoolVolumeTypeCustom,
		s.poolID)
	if err != nil {
		logger.Errorf(`Failed to delete database entry for ZFS `+
			`storage volume "%s" on storage pool "%s"`,
			s.volume.Name, s.pool.Name)
	}

	logger.Infof("Deleted ZFS storage volume \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)
	return nil
}

func (s *storageZfs) StoragePoolVolumeMount() (bool, error) {
	logger.Debugf("Mounting ZFS storage volume \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)

	fs := fmt.Sprintf("custom/%s", s.volume.Name)
	customPoolVolumeMntPoint := getStoragePoolVolumeMountPoint(s.pool.Name, s.volume.Name)

	customMountLockID := getCustomMountLockID(s.pool.Name, s.volume.Name)
	lxdStorageMapLock.Lock()
	if waitChannel, ok := lxdStorageOngoingOperationMap[customMountLockID]; ok {
		lxdStorageMapLock.Unlock()
		if _, ok := <-waitChannel; ok {
			logger.Warnf("Received value over semaphore. This should not have happened.")
		}
		// Give the benefit of the doubt and assume that the other
		// thread actually succeeded in mounting the storage volume.
		return false, nil
	}

	lxdStorageOngoingOperationMap[customMountLockID] = make(chan bool)
	lxdStorageMapLock.Unlock()

	var customerr error
	ourMount := false
	if !shared.IsMountPoint(customPoolVolumeMntPoint) {
		customerr = zfsMount(s.getOnDiskPoolName(), fs)
		ourMount = true
	}

	lxdStorageMapLock.Lock()
	if waitChannel, ok := lxdStorageOngoingOperationMap[customMountLockID]; ok {
		close(waitChannel)
		delete(lxdStorageOngoingOperationMap, customMountLockID)
	}
	lxdStorageMapLock.Unlock()

	if customerr != nil {
		return false, customerr
	}

	logger.Debugf("Mounted ZFS storage volume \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)
	return ourMount, nil
}

func (s *storageZfs) StoragePoolVolumeUmount() (bool, error) {
	logger.Debugf("Unmounting ZFS storage volume \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)

	fs := fmt.Sprintf("custom/%s", s.volume.Name)
	customPoolVolumeMntPoint := getStoragePoolVolumeMountPoint(s.pool.Name, s.volume.Name)

	customUmountLockID := getCustomUmountLockID(s.pool.Name, s.volume.Name)
	lxdStorageMapLock.Lock()
	if waitChannel, ok := lxdStorageOngoingOperationMap[customUmountLockID]; ok {
		lxdStorageMapLock.Unlock()
		if _, ok := <-waitChannel; ok {
			logger.Warnf("Received value over semaphore. This should not have happened.")
		}
		// Give the benefit of the doubt and assume that the other
		// thread actually succeeded in unmounting the storage volume.
		return false, nil
	}

	lxdStorageOngoingOperationMap[customUmountLockID] = make(chan bool)
	lxdStorageMapLock.Unlock()

	var customerr error
	ourUmount := false
	if shared.IsMountPoint(customPoolVolumeMntPoint) {
		customerr = zfsUmount(s.getOnDiskPoolName(), fs, customPoolVolumeMntPoint)
		ourUmount = true
	}

	lxdStorageMapLock.Lock()
	if waitChannel, ok := lxdStorageOngoingOperationMap[customUmountLockID]; ok {
		close(waitChannel)
		delete(lxdStorageOngoingOperationMap, customUmountLockID)
	}
	lxdStorageMapLock.Unlock()

	if customerr != nil {
		return false, customerr
	}

	logger.Debugf("Unmounted ZFS storage volume \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)
	return ourUmount, nil
}

func (s *storageZfs) GetStoragePoolWritable() api.StoragePoolPut {
	return s.pool.Writable()
}

func (s *storageZfs) GetStoragePoolVolumeWritable() api.StorageVolumePut {
	return s.volume.Writable()
}

func (s *storageZfs) SetStoragePoolWritable(writable *api.StoragePoolPut) {
	s.pool.StoragePoolPut = *writable
}

func (s *storageZfs) SetStoragePoolVolumeWritable(writable *api.StorageVolumePut) {
	s.volume.StorageVolumePut = *writable
}

func (s *storageZfs) GetContainerPoolInfo() (int64, string, string) {
	return s.poolID, s.pool.Name, s.getOnDiskPoolName()
}

func (s *storageZfs) StoragePoolUpdate(writable *api.StoragePoolPut, changedConfig []string) error {
	logger.Infof(`Updating ZFS storage pool "%s"`, s.pool.Name)

	changeable := changeableStoragePoolProperties["zfs"]
	unchangeable := []string{}
	for _, change := range changedConfig {
		if !shared.StringInSlice(change, changeable) {
			unchangeable = append(unchangeable, change)
		}
	}

	if len(unchangeable) > 0 {
		return updateStoragePoolError(unchangeable, "zfs")
	}

	// "rsync.bwlimit" requires no on-disk modifications.
	// "volume.zfs.remove_snapshots" requires no on-disk modifications.
	// "volume.zfs.use_refquota" requires no on-disk modifications.

	logger.Infof(`Updated ZFS storage pool "%s"`, s.pool.Name)
	return nil
}

func (s *storageZfs) StoragePoolVolumeUpdate(writable *api.StorageVolumePut, changedConfig []string) error {
	logger.Infof(`Updating ZFS storage volume "%s"`, s.pool.Name)

	changeable := changeableStoragePoolVolumeProperties["zfs"]
	unchangeable := []string{}
	for _, change := range changedConfig {
		if !shared.StringInSlice(change, changeable) {
			unchangeable = append(unchangeable, change)
		}
	}

	if len(unchangeable) > 0 {
		return updateStoragePoolVolumeError(unchangeable, "zfs")
	}

	if shared.StringInSlice("size", changedConfig) {
		if s.volume.Type != storagePoolVolumeTypeNameCustom {
			return updateStoragePoolVolumeError([]string{"size"}, "zfs")
		}

		if s.volume.Config["size"] != writable.Config["size"] {
			size, err := shared.ParseByteSizeString(writable.Config["size"])
			if err != nil {
				return err
			}

			err = s.StorageEntitySetQuota(storagePoolVolumeTypeCustom, size, nil)
			if err != nil {
				return err
			}
		}
	}

	logger.Infof(`Updated ZFS storage volume "%s"`, s.pool.Name)
	return nil
}

// Things we don't need to care about
func (s *storageZfs) ContainerMount(c container) (bool, error) {
	name := c.Name()
	logger.Debugf("Mounting ZFS storage volume for container \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)

	fs := fmt.Sprintf("containers/%s", name)
	containerPoolVolumeMntPoint := getContainerMountPoint(s.pool.Name, name)

	containerMountLockID := getContainerMountLockID(s.pool.Name, name)
	lxdStorageMapLock.Lock()
	if waitChannel, ok := lxdStorageOngoingOperationMap[containerMountLockID]; ok {
		lxdStorageMapLock.Unlock()
		if _, ok := <-waitChannel; ok {
			logger.Warnf("Received value over semaphore. This should not have happened.")
		}
		// Give the benefit of the doubt and assume that the other
		// thread actually succeeded in mounting the storage volume.
		return false, nil
	}

	lxdStorageOngoingOperationMap[containerMountLockID] = make(chan bool)
	lxdStorageMapLock.Unlock()

	removeLockFromMap := func() {
		lxdStorageMapLock.Lock()
		if waitChannel, ok := lxdStorageOngoingOperationMap[containerMountLockID]; ok {
			close(waitChannel)
			delete(lxdStorageOngoingOperationMap, containerMountLockID)
		}
		lxdStorageMapLock.Unlock()
	}

	defer removeLockFromMap()

	// Since we're using mount() directly zfs will not automatically create
	// the mountpoint for us. So let's check and do it if needed.
	if !shared.PathExists(containerPoolVolumeMntPoint) {
		err := createContainerMountpoint(containerPoolVolumeMntPoint, c.Path(), c.IsPrivileged())
		if err != nil {
			return false, err
		}
	}

	ourMount := false
	if !shared.IsMountPoint(containerPoolVolumeMntPoint) {
		source := fmt.Sprintf("%s/%s", s.getOnDiskPoolName(), fs)
		zfsMountOptions := fmt.Sprintf("rw,zfsutil,mntpoint=%s", containerPoolVolumeMntPoint)
		mounterr := tryMount(source, containerPoolVolumeMntPoint, "zfs", 0, zfsMountOptions)
		if mounterr != nil {
			if mounterr != syscall.EBUSY {
				logger.Errorf("Failed to mount ZFS dataset \"%s\" onto \"%s\".", source, containerPoolVolumeMntPoint)
				return false, mounterr
			}
			// EBUSY error in zfs are related to a bug we're
			// tracking. So ignore them for now, report back that
			// the mount isn't ours and proceed.
			logger.Warnf("ZFS returned EBUSY while \"%s\" is actually not a mountpoint.", containerPoolVolumeMntPoint)
			return false, mounterr
		}
		ourMount = true
	}

	logger.Debugf("Mounted ZFS storage volume for container \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)
	return ourMount, nil
}

func (s *storageZfs) ContainerUmount(name string, path string) (bool, error) {
	logger.Debugf("Unmounting ZFS storage volume for container \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)

	fs := fmt.Sprintf("containers/%s", name)
	containerPoolVolumeMntPoint := getContainerMountPoint(s.pool.Name, name)

	containerUmountLockID := getContainerUmountLockID(s.pool.Name, name)
	lxdStorageMapLock.Lock()
	if waitChannel, ok := lxdStorageOngoingOperationMap[containerUmountLockID]; ok {
		lxdStorageMapLock.Unlock()
		if _, ok := <-waitChannel; ok {
			logger.Warnf("Received value over semaphore. This should not have happened.")
		}
		// Give the benefit of the doubt and assume that the other
		// thread actually succeeded in unmounting the storage volume.
		return false, nil
	}

	lxdStorageOngoingOperationMap[containerUmountLockID] = make(chan bool)
	lxdStorageMapLock.Unlock()

	var imgerr error
	ourUmount := false
	if shared.IsMountPoint(containerPoolVolumeMntPoint) {
		imgerr = zfsUmount(s.getOnDiskPoolName(), fs, containerPoolVolumeMntPoint)
		ourUmount = true
	}

	lxdStorageMapLock.Lock()
	if waitChannel, ok := lxdStorageOngoingOperationMap[containerUmountLockID]; ok {
		close(waitChannel)
		delete(lxdStorageOngoingOperationMap, containerUmountLockID)
	}
	lxdStorageMapLock.Unlock()

	if imgerr != nil {
		return false, imgerr
	}

	logger.Debugf("Unmounted ZFS storage volume for container \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)
	return ourUmount, nil
}

// Things we do have to care about
func (s *storageZfs) ContainerStorageReady(name string) bool {
	fs := fmt.Sprintf("containers/%s", name)
	return zfsFilesystemEntityExists(s.getOnDiskPoolName(), fs)
}

func (s *storageZfs) ContainerCreate(container container) error {
	logger.Debugf("Creating empty ZFS storage volume for container \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)

	containerPath := container.Path()
	containerName := container.Name()
	fs := fmt.Sprintf("containers/%s", containerName)
	poolName := s.getOnDiskPoolName()
	dataset := fmt.Sprintf("%s/%s", poolName, fs)
	containerPoolVolumeMntPoint := getContainerMountPoint(s.pool.Name, containerName)

	// Create volume.
	msg, err := zfsPoolVolumeCreate(dataset, "mountpoint=none", "canmount=noauto")
	if err != nil {
		logger.Errorf("failed to create ZFS storage volume for container \"%s\" on storage pool \"%s\": %s", s.volume.Name, s.pool.Name, msg)
		return err
	}

	revert := true
	defer func() {
		if !revert {
			return
		}
		s.ContainerDelete(container)
	}()

	// Set mountpoint.
	err = zfsPoolVolumeSet(poolName, fs, "mountpoint", containerPoolVolumeMntPoint)
	if err != nil {
		return err
	}

	ourMount, err := s.ContainerMount(container)
	if err != nil {
		return err
	}
	if ourMount {
		defer s.ContainerUmount(containerName, containerPath)
	}

	err = createContainerMountpoint(containerPoolVolumeMntPoint, containerPath, container.IsPrivileged())
	if err != nil {
		return err
	}

	err = container.TemplateApply("create")
	if err != nil {
		return err
	}

	revert = false

	logger.Debugf("Created empty ZFS storage volume for container \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)
	return nil
}

func (s *storageZfs) ContainerCreateFromImage(container container, fingerprint string) error {
	logger.Debugf("Creating ZFS storage volume for container \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)

	containerPath := container.Path()
	containerName := container.Name()
	fs := fmt.Sprintf("containers/%s", containerName)
	containerPoolVolumeMntPoint := getContainerMountPoint(s.pool.Name, containerName)

	poolName := s.getOnDiskPoolName()
	fsImage := fmt.Sprintf("images/%s", fingerprint)

	imageStoragePoolLockID := getImageCreateLockID(s.pool.Name, fingerprint)
	lxdStorageMapLock.Lock()
	if waitChannel, ok := lxdStorageOngoingOperationMap[imageStoragePoolLockID]; ok {
		lxdStorageMapLock.Unlock()
		if _, ok := <-waitChannel; ok {
			logger.Warnf("Received value over semaphore. This should not have happened.")
		}
	} else {
		lxdStorageOngoingOperationMap[imageStoragePoolLockID] = make(chan bool)
		lxdStorageMapLock.Unlock()

		var imgerr error
		if !zfsFilesystemEntityExists(poolName, fsImage) {
			imgerr = s.ImageCreate(fingerprint)
		}

		lxdStorageMapLock.Lock()
		if waitChannel, ok := lxdStorageOngoingOperationMap[imageStoragePoolLockID]; ok {
			close(waitChannel)
			delete(lxdStorageOngoingOperationMap, imageStoragePoolLockID)
		}
		lxdStorageMapLock.Unlock()

		if imgerr != nil {
			return imgerr
		}
	}

	err := zfsPoolVolumeClone(poolName, fsImage, "readonly", fs, containerPoolVolumeMntPoint)
	if err != nil {
		return err
	}

	revert := true
	defer func() {
		if !revert {
			return
		}
		s.ContainerDelete(container)
	}()

	ourMount, err := s.ContainerMount(container)
	if err != nil {
		return err
	}
	if ourMount {
		defer s.ContainerUmount(containerName, containerPath)
	}

	privileged := container.IsPrivileged()
	err = createContainerMountpoint(containerPoolVolumeMntPoint, containerPath, privileged)
	if err != nil {
		return err
	}

	if !privileged {
		err = s.shiftRootfs(container)
		if err != nil {
			return err
		}
	}

	err = container.TemplateApply("create")
	if err != nil {
		return err
	}

	revert = false

	logger.Debugf("Created ZFS storage volume for container \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)
	return nil
}

func (s *storageZfs) ContainerCanRestore(container container, sourceContainer container) error {
	snaps, err := container.Snapshots()
	if err != nil {
		return err
	}

	if snaps[len(snaps)-1].Name() != sourceContainer.Name() {
		if s.pool.Config["volume.zfs.remove_snapshots"] != "" {
			zfsRemoveSnapshots = s.pool.Config["volume.zfs.remove_snapshots"]
		}
		if s.volume.Config["zfs.remove_snapshots"] != "" {
			zfsRemoveSnapshots = s.volume.Config["zfs.remove_snapshots"]
		}
		if !shared.IsTrue(zfsRemoveSnapshots) {
			return fmt.Errorf("ZFS can only restore from the latest snapshot. Delete newer snapshots or copy the snapshot into a new container instead")
		}

		return nil
	}

	return nil
}

func (s *storageZfs) ContainerDelete(container container) error {
	logger.Debugf("Deleting ZFS storage volume for container \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)

	poolName := s.getOnDiskPoolName()
	containerName := container.Name()
	fs := fmt.Sprintf("containers/%s", containerName)
	containerPoolVolumeMntPoint := getContainerMountPoint(s.pool.Name, containerName)

	if zfsFilesystemEntityExists(poolName, fs) {
		removable := true
		snaps, err := zfsPoolListSnapshots(poolName, fs)
		if err != nil {
			return err
		}

		for _, snap := range snaps {
			var err error
			removable, err = zfsPoolVolumeSnapshotRemovable(poolName, fs, snap)
			if err != nil {
				return err
			}

			if !removable {
				break
			}
		}

		if removable {
			origin, err := zfsFilesystemEntityPropertyGet(poolName, fs, "origin")
			if err != nil {
				return err
			}
			poolName := s.getOnDiskPoolName()
			origin = strings.TrimPrefix(origin, fmt.Sprintf("%s/", poolName))

			err = zfsPoolVolumeDestroy(poolName, fs)
			if err != nil {
				return err
			}

			err = zfsPoolVolumeCleanup(poolName, origin)
			if err != nil {
				return err
			}
		} else {
			err := zfsPoolVolumeSet(poolName, fs, "mountpoint", "none")
			if err != nil {
				return err
			}

			err = zfsPoolVolumeRename(poolName, fs, fmt.Sprintf("deleted/containers/%s", uuid.NewRandom().String()))
			if err != nil {
				return err
			}
		}
	}

	err := deleteContainerMountpoint(containerPoolVolumeMntPoint, container.Path(), s.GetStorageTypeName())
	if err != nil {
		return err
	}

	snapshotZfsDataset := fmt.Sprintf("snapshots/%s", containerName)
	zfsPoolVolumeDestroy(poolName, snapshotZfsDataset)

	// Delete potential leftover snapshot mountpoints.
	snapshotMntPoint := getSnapshotMountPoint(s.pool.Name, containerName)
	if shared.PathExists(snapshotMntPoint) {
		err := os.RemoveAll(snapshotMntPoint)
		if err != nil {
			return err
		}
	}

	// Delete potential leftover snapshot symlinks:
	// ${LXD_DIR}/snapshots/<container_name> -> ${POOL}/snapshots/<container_name>
	snapshotSymlink := shared.VarPath("snapshots", containerName)
	if shared.PathExists(snapshotSymlink) {
		err := os.Remove(snapshotSymlink)
		if err != nil {
			return err
		}
	}

	logger.Debugf("Deleted ZFS storage volume for container \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)
	return nil
}

func (s *storageZfs) copyWithoutSnapshotsSparse(target container, source container) error {
	poolName := s.getOnDiskPoolName()

	sourceContainerName := source.Name()
	sourceContainerPath := source.Path()

	targetContainerName := target.Name()
	targetContainerPath := target.Path()
	targetContainerMountPoint := getContainerMountPoint(s.pool.Name, targetContainerName)

	sourceZfsDataset := ""
	sourceZfsDatasetSnapshot := ""
	sourceName, sourceSnapOnlyName, isSnapshotName := containerGetParentAndSnapshotName(sourceContainerName)

	targetZfsDataset := fmt.Sprintf("containers/%s", targetContainerName)

	if isSnapshotName {
		sourceZfsDatasetSnapshot = sourceSnapOnlyName
	}

	revert := true
	if sourceZfsDatasetSnapshot == "" {
		if zfsFilesystemEntityExists(poolName, fmt.Sprintf("containers/%s", sourceName)) {
			sourceZfsDatasetSnapshot = fmt.Sprintf("copy-%s", uuid.NewRandom().String())
			sourceZfsDataset = fmt.Sprintf("containers/%s", sourceName)
			err := zfsPoolVolumeSnapshotCreate(poolName, sourceZfsDataset, sourceZfsDatasetSnapshot)
			if err != nil {
				return err
			}
			defer func() {
				if !revert {
					return
				}
				zfsPoolVolumeSnapshotDestroy(poolName, sourceZfsDataset, sourceZfsDatasetSnapshot)
			}()
		}
	} else {
		if zfsFilesystemEntityExists(poolName, fmt.Sprintf("containers/%s@snapshot-%s", sourceName, sourceZfsDatasetSnapshot)) {
			sourceZfsDataset = fmt.Sprintf("containers/%s", sourceName)
			sourceZfsDatasetSnapshot = fmt.Sprintf("snapshot-%s", sourceZfsDatasetSnapshot)
		}
	}

	if sourceZfsDataset != "" {
		err := zfsPoolVolumeClone(poolName, sourceZfsDataset, sourceZfsDatasetSnapshot, targetZfsDataset, targetContainerMountPoint)
		if err != nil {
			return err
		}
		defer func() {
			if !revert {
				return
			}
			zfsPoolVolumeDestroy(poolName, targetZfsDataset)
		}()

		ourMount, err := s.ContainerMount(target)
		if err != nil {
			return err
		}
		if ourMount {
			defer s.ContainerUmount(targetContainerName, targetContainerPath)
		}

		err = createContainerMountpoint(targetContainerMountPoint, targetContainerPath, target.IsPrivileged())
		if err != nil {
			return err
		}
		defer func() {
			if !revert {
				return
			}
			deleteContainerMountpoint(targetContainerMountPoint, targetContainerPath, s.GetStorageTypeName())
		}()
	} else {
		err := s.ContainerCreate(target)
		if err != nil {
			return err
		}
		defer func() {
			if !revert {
				return
			}
			s.ContainerDelete(target)
		}()

		bwlimit := s.pool.Config["rsync.bwlimit"]
		output, err := rsyncLocalCopy(sourceContainerPath, targetContainerPath, bwlimit)
		if err != nil {
			return fmt.Errorf("rsync failed: %s", string(output))
		}
	}

	err := target.TemplateApply("copy")
	if err != nil {
		return err
	}

	revert = false

	return nil
}

func (s *storageZfs) copyWithoutSnapshotFull(target container, source container) error {
	logger.Debugf("Creating full ZFS copy \"%s\" -> \"%s\".", source.Name(), target.Name())

	sourceIsSnapshot := source.IsSnapshot()
	poolName := s.getOnDiskPoolName()

	sourceName := source.Name()
	sourceDataset := ""
	snapshotSuffix := ""

	targetName := target.Name()
	targetDataset := fmt.Sprintf("%s/containers/%s", poolName, targetName)
	targetSnapshotDataset := targetDataset

	if sourceIsSnapshot {
		sourceParentName, sourceSnapOnlyName, _ := containerGetParentAndSnapshotName(source.Name())
		snapshotSuffix = fmt.Sprintf("snapshot-%s", sourceSnapOnlyName)
		sourceDataset = fmt.Sprintf("%s/containers/%s@%s", poolName, sourceParentName, snapshotSuffix)
		targetSnapshotDataset = fmt.Sprintf("%s/containers/%s@snapshot-%s", poolName, targetName, sourceSnapOnlyName)
	} else {
		snapshotSuffix = uuid.NewRandom().String()
		sourceDataset = fmt.Sprintf("%s/containers/%s@%s", poolName, sourceName, snapshotSuffix)
		targetSnapshotDataset = fmt.Sprintf("%s/containers/%s@%s", poolName, targetName, snapshotSuffix)

		fs := fmt.Sprintf("containers/%s", sourceName)
		err := zfsPoolVolumeSnapshotCreate(poolName, fs, snapshotSuffix)
		if err != nil {
			return err
		}
		defer func() {
			err := zfsPoolVolumeSnapshotDestroy(poolName, fs, snapshotSuffix)
			if err != nil {
				logger.Warnf("Failed to delete temporary ZFS snapshot \"%s\". Manual cleanup needed.", sourceDataset)
			}
		}()
	}

	zfsSendCmd := exec.Command("zfs", "send", sourceDataset)

	zfsRecvCmd := exec.Command("zfs", "receive", targetDataset)

	zfsRecvCmd.Stdin, _ = zfsSendCmd.StdoutPipe()
	zfsRecvCmd.Stdout = os.Stdout
	zfsRecvCmd.Stderr = os.Stderr

	err := zfsRecvCmd.Start()
	if err != nil {
		return err
	}

	err = zfsSendCmd.Run()
	if err != nil {
		return err
	}

	err = zfsRecvCmd.Wait()
	if err != nil {
		return err
	}

	msg, err := shared.RunCommand("zfs", "rollback", "-r", "-R", targetSnapshotDataset)
	if err != nil {
		logger.Errorf("Failed to rollback ZFS dataset: %s.", msg)
		return err
	}

	targetContainerMountPoint := getContainerMountPoint(s.pool.Name, targetName)
	targetfs := fmt.Sprintf("containers/%s", targetName)

	err = zfsPoolVolumeSet(poolName, targetfs, "canmount", "noauto")
	if err != nil {
		return err
	}

	err = zfsPoolVolumeSet(poolName, targetfs, "mountpoint", targetContainerMountPoint)
	if err != nil {
		return err
	}

	err = zfsPoolVolumeSnapshotDestroy(poolName, targetfs, snapshotSuffix)
	if err != nil {
		return err
	}

	ourMount, err := s.ContainerMount(target)
	if err != nil {
		return err
	}
	if ourMount {
		defer s.ContainerUmount(targetName, targetContainerMountPoint)
	}

	err = createContainerMountpoint(targetContainerMountPoint, target.Path(), target.IsPrivileged())
	if err != nil {
		return err
	}

	logger.Debugf("Created full ZFS copy \"%s\" -> \"%s\".", source.Name(), target.Name())
	return nil
}

func (s *storageZfs) copyWithSnapshots(target container, source container, parentSnapshot string) error {
	sourceName := source.Name()
	targetParentName, targetSnapOnlyName, _ := containerGetParentAndSnapshotName(target.Name())
	containersPath := getSnapshotMountPoint(s.pool.Name, targetParentName)
	snapshotMntPointSymlinkTarget := shared.VarPath("storage-pools", s.pool.Name, "snapshots", targetParentName)
	snapshotMntPointSymlink := shared.VarPath("snapshots", targetParentName)
	err := createSnapshotMountpoint(containersPath, snapshotMntPointSymlinkTarget, snapshotMntPointSymlink)
	if err != nil {
		return err
	}

	poolName := s.getOnDiskPoolName()
	sourceParentName, sourceSnapOnlyName, _ := containerGetParentAndSnapshotName(sourceName)
	currentSnapshotDataset := fmt.Sprintf("%s/containers/%s@snapshot-%s", poolName, sourceParentName, sourceSnapOnlyName)
	args := []string{"send", currentSnapshotDataset}
	if parentSnapshot != "" {
		parentName, parentSnaponlyName, _ := containerGetParentAndSnapshotName(parentSnapshot)
		parentSnapshotDataset := fmt.Sprintf("%s/containers/%s@snapshot-%s", poolName, parentName, parentSnaponlyName)
		args = append(args, "-i", parentSnapshotDataset)
	}

	zfsSendCmd := exec.Command("zfs", args...)
	targetSnapshotDataset := fmt.Sprintf("%s/containers/%s@snapshot-%s", poolName, targetParentName, targetSnapOnlyName)
	zfsRecvCmd := exec.Command("zfs", "receive", "-F", targetSnapshotDataset)

	zfsRecvCmd.Stdin, _ = zfsSendCmd.StdoutPipe()
	zfsRecvCmd.Stdout = os.Stdout
	zfsRecvCmd.Stderr = os.Stderr

	err = zfsRecvCmd.Start()
	if err != nil {
		return err
	}

	err = zfsSendCmd.Run()
	if err != nil {
		return err
	}

	err = zfsRecvCmd.Wait()
	if err != nil {
		return err
	}

	return nil
}

func (s *storageZfs) ContainerCopy(target container, source container, containerOnly bool) error {
	logger.Debugf("Copying ZFS container storage %s -> %s.", source.Name(), target.Name())

	ourStart, err := source.StorageStart()
	if err != nil {
		return err
	}
	if ourStart {
		defer source.StorageStop()
	}

	_, sourcePool, _ := source.Storage().GetContainerPoolInfo()
	_, targetPool, _ := target.Storage().GetContainerPoolInfo()
	if sourcePool != targetPool {
		return fmt.Errorf("copying containers between different storage pools is not implemented")
	}

	snapshots, err := source.Snapshots()
	if err != nil {
		return err
	}

	if containerOnly || len(snapshots) == 0 {
		if s.pool.Config["zfs.clone_copy"] != "" && !shared.IsTrue(s.pool.Config["zfs.clone_copy"]) {
			err = s.copyWithoutSnapshotFull(target, source)
		} else {
			err = s.copyWithoutSnapshotsSparse(target, source)
		}
	} else {
		targetContainerName := target.Name()
		targetContainerPath := target.Path()
		targetContainerMountPoint := getContainerMountPoint(s.pool.Name, targetContainerName)
		err = createContainerMountpoint(targetContainerMountPoint, targetContainerPath, target.IsPrivileged())
		if err != nil {
			return err
		}

		prev := ""
		prevSnapOnlyName := ""
		for i, snap := range snapshots {
			if i > 0 {
				prev = snapshots[i-1].Name()
			}

			sourceSnapshot, err := containerLoadByName(s.s, snap.Name())
			if err != nil {
				return err
			}

			_, snapOnlyName, _ := containerGetParentAndSnapshotName(snap.Name())
			prevSnapOnlyName = snapOnlyName
			newSnapName := fmt.Sprintf("%s/%s", target.Name(), snapOnlyName)
			targetSnapshot, err := containerLoadByName(s.s, newSnapName)
			if err != nil {
				return err
			}

			err = s.copyWithSnapshots(targetSnapshot, sourceSnapshot, prev)
			if err != nil {
				return err
			}
		}

		poolName := s.getOnDiskPoolName()

		// send actual container
		tmpSnapshotName := fmt.Sprintf("copy-send-%s", uuid.NewRandom().String())
		err = zfsPoolVolumeSnapshotCreate(poolName, fmt.Sprintf("containers/%s", source.Name()), tmpSnapshotName)
		if err != nil {
			return err
		}

		currentSnapshotDataset := fmt.Sprintf("%s/containers/%s@%s", poolName, source.Name(), tmpSnapshotName)
		args := []string{"send", currentSnapshotDataset}
		if prevSnapOnlyName != "" {
			parentSnapshotDataset := fmt.Sprintf("%s/containers/%s@snapshot-%s", poolName, source.Name(), prevSnapOnlyName)
			args = append(args, "-i", parentSnapshotDataset)
		}

		zfsSendCmd := exec.Command("zfs", args...)
		targetSnapshotDataset := fmt.Sprintf("%s/containers/%s@%s", poolName, target.Name(), tmpSnapshotName)
		zfsRecvCmd := exec.Command("zfs", "receive", "-F", targetSnapshotDataset)

		zfsRecvCmd.Stdin, _ = zfsSendCmd.StdoutPipe()
		zfsRecvCmd.Stdout = os.Stdout
		zfsRecvCmd.Stderr = os.Stderr

		err = zfsRecvCmd.Start()
		if err != nil {
			return err
		}

		err = zfsSendCmd.Run()
		if err != nil {
			return err
		}

		err = zfsRecvCmd.Wait()
		if err != nil {
			return err
		}

		zfsPoolVolumeSnapshotDestroy(poolName, fmt.Sprintf("containers/%s", source.Name()), tmpSnapshotName)
		zfsPoolVolumeSnapshotDestroy(poolName, fmt.Sprintf("containers/%s", target.Name()), tmpSnapshotName)

		fs := fmt.Sprintf("containers/%s", target.Name())
		err = zfsPoolVolumeSet(poolName, fs, "canmount", "noauto")
		if err != nil {
			return err
		}

		err = zfsPoolVolumeSet(poolName, fs, "mountpoint", targetContainerMountPoint)
		if err != nil {
			return err
		}

	}

	logger.Debugf("Copied ZFS container storage %s -> %s.", source.Name(), target.Name())
	return nil
}

func (s *storageZfs) ContainerRename(container container, newName string) error {
	logger.Debugf("Renaming ZFS storage volume for container \"%s\" from %s -> %s.", s.volume.Name, s.volume.Name, newName)

	poolName := s.getOnDiskPoolName()
	oldName := container.Name()

	// Unmount the dataset.
	_, err := s.ContainerUmount(oldName, "")
	if err != nil {
		return err
	}

	// Rename the dataset.
	oldZfsDataset := fmt.Sprintf("containers/%s", oldName)
	newZfsDataset := fmt.Sprintf("containers/%s", newName)
	err = zfsPoolVolumeRename(poolName, oldZfsDataset, newZfsDataset)
	if err != nil {
		return err
	}
	revert := true
	defer func() {
		if !revert {
			return
		}
		s.ContainerRename(container, oldName)
	}()

	// Set the new mountpoint for the dataset.
	newContainerMntPoint := getContainerMountPoint(s.pool.Name, newName)
	err = zfsPoolVolumeSet(poolName, newZfsDataset, "mountpoint", newContainerMntPoint)
	if err != nil {
		return err
	}

	// Unmount the dataset.
	_, err = s.ContainerUmount(newName, "")
	if err != nil {
		return err
	}

	// Create new mountpoint on the storage pool.
	oldContainerMntPoint := getContainerMountPoint(s.pool.Name, oldName)
	oldContainerMntPointSymlink := container.Path()
	newContainerMntPointSymlink := shared.VarPath("containers", newName)
	err = renameContainerMountpoint(oldContainerMntPoint, oldContainerMntPointSymlink, newContainerMntPoint, newContainerMntPointSymlink)
	if err != nil {
		return err
	}

	// Rename the snapshot mountpoint on the storage pool.
	oldSnapshotMntPoint := getSnapshotMountPoint(s.pool.Name, oldName)
	newSnapshotMntPoint := getSnapshotMountPoint(s.pool.Name, newName)
	if shared.PathExists(oldSnapshotMntPoint) {
		err := os.Rename(oldSnapshotMntPoint, newSnapshotMntPoint)
		if err != nil {
			return err
		}
	}

	// Remove old symlink.
	oldSnapshotPath := shared.VarPath("snapshots", oldName)
	if shared.PathExists(oldSnapshotPath) {
		err := os.Remove(oldSnapshotPath)
		if err != nil {
			return err
		}
	}

	// Create new symlink.
	newSnapshotPath := shared.VarPath("snapshots", newName)
	if shared.PathExists(newSnapshotPath) {
		err := os.Symlink(newSnapshotMntPoint, newSnapshotPath)
		if err != nil {
			return err
		}
	}

	revert = false

	logger.Debugf("Renamed ZFS storage volume for container \"%s\" from %s -> %s.", s.volume.Name, s.volume.Name, newName)
	return nil
}

func (s *storageZfs) ContainerRestore(target container, source container) error {
	logger.Debugf("Restoring ZFS storage volume for container \"%s\" from %s -> %s.", s.volume.Name, source.Name(), target.Name())

	// Start storage for source container
	ourSourceStart, err := source.StorageStart()
	if err != nil {
		return err
	}
	if ourSourceStart {
		defer source.StorageStop()
	}

	// Start storage for target container
	ourTargetStart, err := target.StorageStart()
	if err != nil {
		return err
	}
	if ourTargetStart {
		defer target.StorageStop()
	}

	// Remove any needed snapshot
	snaps, err := target.Snapshots()
	if err != nil {
		return err
	}

	for i := len(snaps) - 1; i != 0; i-- {
		if snaps[i].Name() == source.Name() {
			break
		}

		err := snaps[i].Delete()
		if err != nil {
			return err
		}
	}

	// Restore the snapshot
	cName, snapOnlyName, _ := containerGetParentAndSnapshotName(source.Name())
	snapName := fmt.Sprintf("snapshot-%s", snapOnlyName)

	err = zfsPoolVolumeSnapshotRestore(s.getOnDiskPoolName(), fmt.Sprintf("containers/%s", cName), snapName)
	if err != nil {
		return err
	}

	logger.Debugf("Restored ZFS storage volume for container \"%s\" from %s -> %s.", s.volume.Name, source.Name(), target.Name())
	return nil
}

func (s *storageZfs) ContainerGetUsage(container container) (int64, error) {
	var err error

	fs := fmt.Sprintf("containers/%s", container.Name())

	property := "used"

	if s.pool.Config["volume.zfs.use_refquota"] != "" {
		zfsUseRefquota = s.pool.Config["volume.zfs.use_refquota"]
	}
	if s.volume.Config["zfs.use_refquota"] != "" {
		zfsUseRefquota = s.volume.Config["zfs.use_refquota"]
	}

	if shared.IsTrue(zfsUseRefquota) {
		property = "referenced"
	}

	value, err := zfsFilesystemEntityPropertyGet(s.getOnDiskPoolName(), fs, property)
	if err != nil {
		return -1, err
	}

	valueInt, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return -1, err
	}

	return valueInt, nil
}

func (s *storageZfs) ContainerSnapshotCreate(snapshotContainer container, sourceContainer container) error {
	snapshotContainerName := snapshotContainer.Name()
	logger.Debugf("Creating ZFS storage volume for snapshot \"%s\" on storage pool \"%s\".", snapshotContainerName, s.pool.Name)

	sourceContainerName := sourceContainer.Name()

	cName, snapshotSnapOnlyName, _ := containerGetParentAndSnapshotName(snapshotContainerName)
	snapName := fmt.Sprintf("snapshot-%s", snapshotSnapOnlyName)

	sourceZfsDataset := fmt.Sprintf("containers/%s", cName)
	err := zfsPoolVolumeSnapshotCreate(s.getOnDiskPoolName(), sourceZfsDataset, snapName)
	if err != nil {
		return err
	}
	revert := true
	defer func() {
		if !revert {
			return
		}
		s.ContainerSnapshotDelete(snapshotContainer)
	}()

	snapshotMntPoint := getSnapshotMountPoint(s.pool.Name, snapshotContainerName)
	if !shared.PathExists(snapshotMntPoint) {
		err := os.MkdirAll(snapshotMntPoint, 0700)
		if err != nil {
			return err
		}
	}

	snapshotMntPointSymlinkTarget := shared.VarPath("storage-pools", s.pool.Name, "snapshots", sourceContainer.Name())
	snapshotMntPointSymlink := shared.VarPath("snapshots", sourceContainerName)
	if !shared.PathExists(snapshotMntPointSymlink) {
		err := os.Symlink(snapshotMntPointSymlinkTarget, snapshotMntPointSymlink)
		if err != nil {
			return err
		}
	}

	revert = false

	logger.Debugf("Created ZFS storage volume for snapshot \"%s\" on storage pool \"%s\".", snapshotContainerName, s.pool.Name)
	return nil
}

func zfsSnapshotDeleteInternal(poolName string, ctName string, onDiskPoolName string) error {
	sourceContainerName, sourceContainerSnapOnlyName, _ := containerGetParentAndSnapshotName(ctName)
	snapName := fmt.Sprintf("snapshot-%s", sourceContainerSnapOnlyName)

	if zfsFilesystemEntityExists(onDiskPoolName,
		fmt.Sprintf("containers/%s@%s",
			sourceContainerName, snapName)) {
		removable, err := zfsPoolVolumeSnapshotRemovable(onDiskPoolName,
			fmt.Sprintf("containers/%s",
				sourceContainerName),
			snapName)
		if err != nil {
			return err
		}

		if removable {
			err = zfsPoolVolumeSnapshotDestroy(onDiskPoolName,
				fmt.Sprintf("containers/%s",
					sourceContainerName),
				snapName)
		} else {
			err = zfsPoolVolumeSnapshotRename(onDiskPoolName,
				fmt.Sprintf("containers/%s",
					sourceContainerName),
				snapName,
				fmt.Sprintf("copy-%s", uuid.NewRandom().String()))
		}
		if err != nil {
			return err
		}
	}

	// Delete the snapshot on its storage pool:
	// ${POOL}/snapshots/<snapshot_name>
	snapshotContainerMntPoint := getSnapshotMountPoint(poolName, ctName)
	if shared.PathExists(snapshotContainerMntPoint) {
		err := os.RemoveAll(snapshotContainerMntPoint)
		if err != nil {
			return err
		}
	}

	// Check if we can remove the snapshot symlink:
	// ${LXD_DIR}/snapshots/<container_name> -> ${POOL}/snapshots/<container_name>
	// by checking if the directory is empty.
	snapshotContainerPath := getSnapshotMountPoint(poolName, sourceContainerName)
	empty, _ := shared.PathIsEmpty(snapshotContainerPath)
	if empty == true {
		// Remove the snapshot directory for the container:
		// ${POOL}/snapshots/<source_container_name>
		err := os.Remove(snapshotContainerPath)
		if err != nil {
			return err
		}

		snapshotSymlink := shared.VarPath("snapshots", sourceContainerName)
		if shared.PathExists(snapshotSymlink) {
			err := os.Remove(snapshotSymlink)
			if err != nil {
				return err
			}
		}
	}

	// Legacy
	snapPath := shared.VarPath(fmt.Sprintf("snapshots/%s/%s.zfs", sourceContainerName, sourceContainerSnapOnlyName))
	if shared.PathExists(snapPath) {
		err := os.Remove(snapPath)
		if err != nil {
			return err
		}
	}

	// Legacy
	parent := shared.VarPath(fmt.Sprintf("snapshots/%s", sourceContainerName))
	if ok, _ := shared.PathIsEmpty(parent); ok {
		err := os.Remove(parent)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *storageZfs) ContainerSnapshotDelete(snapshotContainer container) error {
	logger.Debugf("Deleting ZFS storage volume for snapshot \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)

	poolName := s.getOnDiskPoolName()
	err := zfsSnapshotDeleteInternal(s.pool.Name, snapshotContainer.Name(),
		poolName)
	if err != nil {
		return err
	}

	logger.Debugf("Deleted ZFS storage volume for snapshot \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)
	return nil
}

func (s *storageZfs) ContainerSnapshotRename(snapshotContainer container, newName string) error {
	logger.Debugf("Renaming ZFS storage volume for snapshot \"%s\" from %s -> %s.", s.volume.Name, s.volume.Name, newName)

	oldName := snapshotContainer.Name()

	oldcName, oldSnapOnlyName, _ := containerGetParentAndSnapshotName(snapshotContainer.Name())
	oldZfsDatasetName := fmt.Sprintf("snapshot-%s", oldSnapOnlyName)

	_, newSnapOnlyName, _ := containerGetParentAndSnapshotName(newName)
	newZfsDatasetName := fmt.Sprintf("snapshot-%s", newSnapOnlyName)

	if oldZfsDatasetName != newZfsDatasetName {
		err := zfsPoolVolumeSnapshotRename(
			s.getOnDiskPoolName(), fmt.Sprintf("containers/%s", oldcName), oldZfsDatasetName, newZfsDatasetName)
		if err != nil {
			return err
		}
	}
	revert := true
	defer func() {
		if !revert {
			return
		}
		s.ContainerSnapshotRename(snapshotContainer, oldName)
	}()

	oldStyleSnapshotMntPoint := shared.VarPath(fmt.Sprintf("snapshots/%s/%s.zfs", oldcName, oldSnapOnlyName))
	if shared.PathExists(oldStyleSnapshotMntPoint) {
		err := os.Remove(oldStyleSnapshotMntPoint)
		if err != nil {
			return err
		}
	}

	oldSnapshotMntPoint := getSnapshotMountPoint(s.pool.Name, oldName)
	if shared.PathExists(oldSnapshotMntPoint) {
		err := os.Remove(oldSnapshotMntPoint)
		if err != nil {
			return err
		}
	}

	newSnapshotMntPoint := getSnapshotMountPoint(s.pool.Name, newName)
	if !shared.PathExists(newSnapshotMntPoint) {
		err := os.MkdirAll(newSnapshotMntPoint, 0700)
		if err != nil {
			return err
		}
	}

	snapshotMntPointSymlinkTarget := shared.VarPath("storage-pools", s.pool.Name, "snapshots", oldcName)
	snapshotMntPointSymlink := shared.VarPath("snapshots", oldcName)
	if !shared.PathExists(snapshotMntPointSymlink) {
		err := os.Symlink(snapshotMntPointSymlinkTarget, snapshotMntPointSymlink)
		if err != nil {
			return err
		}
	}

	revert = false

	logger.Debugf("Renamed ZFS storage volume for snapshot \"%s\" from %s -> %s.", s.volume.Name, s.volume.Name, newName)
	return nil
}

func (s *storageZfs) ContainerSnapshotStart(container container) (bool, error) {
	logger.Debugf("Initializing ZFS storage volume for snapshot \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)

	cName, sName, _ := containerGetParentAndSnapshotName(container.Name())
	sourceFs := fmt.Sprintf("containers/%s", cName)
	sourceSnap := fmt.Sprintf("snapshot-%s", sName)
	destFs := fmt.Sprintf("snapshots/%s/%s", cName, sName)

	poolName := s.getOnDiskPoolName()
	snapshotMntPoint := getSnapshotMountPoint(s.pool.Name, container.Name())
	err := zfsPoolVolumeClone(poolName, sourceFs, sourceSnap, destFs, snapshotMntPoint)
	if err != nil {
		return false, err
	}

	err = zfsMount(poolName, destFs)
	if err != nil {
		return false, err
	}

	logger.Debugf("Initialized ZFS storage volume for snapshot \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)
	return true, nil
}

func (s *storageZfs) ContainerSnapshotStop(container container) (bool, error) {
	logger.Debugf("Stopping ZFS storage volume for snapshot \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)

	cName, sName, _ := containerGetParentAndSnapshotName(container.Name())
	destFs := fmt.Sprintf("snapshots/%s/%s", cName, sName)

	err := zfsPoolVolumeDestroy(s.getOnDiskPoolName(), destFs)
	if err != nil {
		return false, err
	}

	logger.Debugf("Stopped ZFS storage volume for snapshot \"%s\" on storage pool \"%s\".", s.volume.Name, s.pool.Name)
	return true, nil
}

func (s *storageZfs) ContainerSnapshotCreateEmpty(snapshotContainer container) error {
	/* don't touch the fs yet, as migration will do that for us */
	return nil
}

// - create temporary directory ${LXD_DIR}/images/lxd_images_
// - create new zfs volume images/<fingerprint>
// - mount the zfs volume on ${LXD_DIR}/images/lxd_images_
// - unpack the downloaded image in ${LXD_DIR}/images/lxd_images_
// - mark new zfs volume images/<fingerprint> readonly
// - remove mountpoint property from zfs volume images/<fingerprint>
// - create read-write snapshot from zfs volume images/<fingerprint>
func (s *storageZfs) ImageCreate(fingerprint string) error {
	logger.Debugf("Creating ZFS storage volume for image \"%s\" on storage pool \"%s\".", fingerprint, s.pool.Name)

	poolName := s.getOnDiskPoolName()
	imageMntPoint := getImageMountPoint(s.pool.Name, fingerprint)
	fs := fmt.Sprintf("images/%s", fingerprint)
	revert := true
	subrevert := true

	err := s.createImageDbPoolVolume(fingerprint)
	if err != nil {
		return err
	}
	defer func() {
		if !subrevert {
			return
		}
		s.deleteImageDbPoolVolume(fingerprint)
	}()

	if zfsFilesystemEntityExists(poolName, fmt.Sprintf("deleted/%s", fs)) {
		if err := zfsPoolVolumeRename(poolName, fmt.Sprintf("deleted/%s", fs), fs); err != nil {
			return err
		}

		defer func() {
			if !revert {
				return
			}
			s.ImageDelete(fingerprint)
		}()

		// In case this is an image from an older lxd instance, wipe the
		// mountpoint.
		err = zfsPoolVolumeSet(poolName, fs, "mountpoint", "none")
		if err != nil {
			return err
		}

		revert = false
		subrevert = false

		return nil
	}

	if !shared.PathExists(imageMntPoint) {
		err := os.MkdirAll(imageMntPoint, 0700)
		if err != nil {
			return err
		}
		defer func() {
			if !subrevert {
				return
			}
			os.RemoveAll(imageMntPoint)
		}()
	}

	// Create temporary mountpoint directory.
	tmp := getImageMountPoint(s.pool.Name, "")
	tmpImageDir, err := ioutil.TempDir(tmp, "")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpImageDir)

	imagePath := shared.VarPath("images", fingerprint)

	// Create a new storage volume on the storage pool for the image.
	dataset := fmt.Sprintf("%s/%s", poolName, fs)
	msg, err := zfsPoolVolumeCreate(dataset, "mountpoint=none")
	if err != nil {
		logger.Errorf("failed to create ZFS dataset \"%s\" on storage pool \"%s\": %s", dataset, s.pool.Name, msg)
		return err
	}
	subrevert = false
	defer func() {
		if !revert {
			return
		}
		s.ImageDelete(fingerprint)
	}()

	// Set a temporary mountpoint for the image.
	err = zfsPoolVolumeSet(poolName, fs, "mountpoint", tmpImageDir)
	if err != nil {
		return err
	}

	// Make sure that the image actually got mounted.
	if !shared.IsMountPoint(tmpImageDir) {
		zfsMount(poolName, fs)
	}

	// Unpack the image into the temporary mountpoint.
	err = unpackImage(imagePath, tmpImageDir, storageTypeZfs)
	if err != nil {
		return err
	}

	// Mark the new storage volume for the image as readonly.
	if err = zfsPoolVolumeSet(poolName, fs, "readonly", "on"); err != nil {
		return err
	}

	// Remove the temporary mountpoint from the image storage volume.
	if err = zfsPoolVolumeSet(poolName, fs, "mountpoint", "none"); err != nil {
		return err
	}

	// Make sure that the image actually got unmounted.
	if shared.IsMountPoint(tmpImageDir) {
		zfsUmount(poolName, fs, tmpImageDir)
	}

	// Create a snapshot of that image on the storage pool which we clone for
	// container creation.
	err = zfsPoolVolumeSnapshotCreate(poolName, fs, "readonly")
	if err != nil {
		return err
	}

	revert = false

	logger.Debugf("Created ZFS storage volume for image \"%s\" on storage pool \"%s\".", fingerprint, s.pool.Name)
	return nil
}

func (s *storageZfs) ImageDelete(fingerprint string) error {
	logger.Debugf("Deleting ZFS storage volume for image \"%s\" on storage pool \"%s\".", fingerprint, s.pool.Name)

	poolName := s.getOnDiskPoolName()
	fs := fmt.Sprintf("images/%s", fingerprint)

	if zfsFilesystemEntityExists(poolName, fs) {
		removable, err := zfsPoolVolumeSnapshotRemovable(poolName, fs, "readonly")
		if err != nil {
			return err
		}

		if removable {
			err := zfsPoolVolumeDestroy(poolName, fs)
			if err != nil {
				return err
			}
		} else {
			if err := zfsPoolVolumeSet(poolName, fs, "mountpoint", "none"); err != nil {
				return err
			}

			if err := zfsPoolVolumeRename(poolName, fs, fmt.Sprintf("deleted/%s", fs)); err != nil {
				return err
			}
		}
	}

	err := s.deleteImageDbPoolVolume(fingerprint)
	if err != nil {
		return err
	}

	imageMntPoint := getImageMountPoint(s.pool.Name, fingerprint)
	if shared.PathExists(imageMntPoint) {
		err := os.RemoveAll(imageMntPoint)
		if err != nil {
			return err
		}
	}

	if shared.PathExists(shared.VarPath(fs + ".zfs")) {
		err := os.RemoveAll(shared.VarPath(fs + ".zfs"))
		if err != nil {
			return err
		}
	}

	logger.Debugf("Deleted ZFS storage volume for image \"%s\" on storage pool \"%s\".", fingerprint, s.pool.Name)
	return nil
}

func (s *storageZfs) ImageMount(fingerprint string) (bool, error) {
	return true, nil
}

func (s *storageZfs) ImageUmount(fingerprint string) (bool, error) {
	return true, nil
}

type zfsMigrationSourceDriver struct {
	container        container
	snapshots        []container
	zfsSnapshotNames []string
	zfs              *storageZfs
	runningSnapName  string
	stoppedSnapName  string
}

func (s *zfsMigrationSourceDriver) Snapshots() []container {
	return s.snapshots
}

func (s *zfsMigrationSourceDriver) send(conn *websocket.Conn, zfsName string, zfsParent string, readWrapper func(io.ReadCloser) io.ReadCloser) error {
	sourceParentName, _, _ := containerGetParentAndSnapshotName(s.container.Name())
	poolName := s.zfs.getOnDiskPoolName()
	args := []string{"send", fmt.Sprintf("%s/containers/%s@%s", poolName, sourceParentName, zfsName)}
	if zfsParent != "" {
		args = append(args, "-i", fmt.Sprintf("%s/containers/%s@%s", poolName, s.container.Name(), zfsParent))
	}

	cmd := exec.Command("zfs", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	readPipe := io.ReadCloser(stdout)
	if readWrapper != nil {
		readPipe = readWrapper(stdout)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	<-shared.WebsocketSendStream(conn, readPipe, 4*1024*1024)

	output, err := ioutil.ReadAll(stderr)
	if err != nil {
		logger.Errorf("Problem reading zfs send stderr: %s.", err)
	}

	err = cmd.Wait()
	if err != nil {
		logger.Errorf("Problem with zfs send: %s.", string(output))
	}

	return err
}

func (s *zfsMigrationSourceDriver) SendWhileRunning(conn *websocket.Conn, op *operation, bwlimit string, containerOnly bool) error {
	if s.container.IsSnapshot() {
		_, snapOnlyName, _ := containerGetParentAndSnapshotName(s.container.Name())
		snapshotName := fmt.Sprintf("snapshot-%s", snapOnlyName)
		wrapper := StorageProgressReader(op, "fs_progress", s.container.Name())
		return s.send(conn, snapshotName, "", wrapper)
	}

	lastSnap := ""
	if !containerOnly {
		for i, snap := range s.zfsSnapshotNames {
			prev := ""
			if i > 0 {
				prev = s.zfsSnapshotNames[i-1]
			}

			lastSnap = snap

			wrapper := StorageProgressReader(op, "fs_progress", snap)
			if err := s.send(conn, snap, prev, wrapper); err != nil {
				return err
			}
		}
	}

	s.runningSnapName = fmt.Sprintf("migration-send-%s", uuid.NewRandom().String())
	if err := zfsPoolVolumeSnapshotCreate(s.zfs.getOnDiskPoolName(), fmt.Sprintf("containers/%s", s.container.Name()), s.runningSnapName); err != nil {
		return err
	}

	wrapper := StorageProgressReader(op, "fs_progress", s.container.Name())
	if err := s.send(conn, s.runningSnapName, lastSnap, wrapper); err != nil {
		return err
	}

	return nil
}

func (s *zfsMigrationSourceDriver) SendAfterCheckpoint(conn *websocket.Conn, bwlimit string) error {
	s.stoppedSnapName = fmt.Sprintf("migration-send-%s", uuid.NewRandom().String())
	if err := zfsPoolVolumeSnapshotCreate(s.zfs.getOnDiskPoolName(), fmt.Sprintf("containers/%s", s.container.Name()), s.stoppedSnapName); err != nil {
		return err
	}

	if err := s.send(conn, s.stoppedSnapName, s.runningSnapName, nil); err != nil {
		return err
	}

	return nil
}

func (s *zfsMigrationSourceDriver) Cleanup() {
	poolName := s.zfs.getOnDiskPoolName()
	if s.stoppedSnapName != "" {
		zfsPoolVolumeSnapshotDestroy(poolName, fmt.Sprintf("containers/%s", s.container.Name()), s.stoppedSnapName)
	}
	if s.runningSnapName != "" {
		zfsPoolVolumeSnapshotDestroy(poolName, fmt.Sprintf("containers/%s", s.container.Name()), s.runningSnapName)
	}
}

func (s *storageZfs) MigrationType() MigrationFSType {
	return MigrationFSType_ZFS
}

func (s *storageZfs) PreservesInodes() bool {
	return true
}

func (s *storageZfs) MigrationSource(ct container, containerOnly bool) (MigrationStorageSourceDriver, error) {
	/* If the container is a snapshot, let's just send that; we don't need
	* to send anything else, because that's all the user asked for.
	 */
	if ct.IsSnapshot() {
		return &zfsMigrationSourceDriver{container: ct, zfs: s}, nil
	}

	driver := zfsMigrationSourceDriver{
		container:        ct,
		snapshots:        []container{},
		zfsSnapshotNames: []string{},
		zfs:              s,
	}

	if containerOnly {
		return &driver, nil
	}

	/* List all the snapshots in order of reverse creation. The idea here
	* is that we send the oldest to newest snapshot, hopefully saving on
	* xfer costs. Then, after all that, we send the container itself.
	 */
	snapshots, err := zfsPoolListSnapshots(s.getOnDiskPoolName(), fmt.Sprintf("containers/%s", ct.Name()))
	if err != nil {
		return nil, err
	}

	for _, snap := range snapshots {
		/* In the case of e.g. multiple copies running at the same
		* time, we will have potentially multiple migration-send
		* snapshots. (Or in the case of the test suite, sometimes one
		* will take too long to delete.)
		 */
		if !strings.HasPrefix(snap, "snapshot-") {
			continue
		}

		lxdName := fmt.Sprintf("%s%s%s", ct.Name(), shared.SnapshotDelimiter, snap[len("snapshot-"):])
		snapshot, err := containerLoadByName(s.s, lxdName)
		if err != nil {
			return nil, err
		}

		driver.snapshots = append(driver.snapshots, snapshot)
		driver.zfsSnapshotNames = append(driver.zfsSnapshotNames, snap)
	}

	return &driver, nil
}

func (s *storageZfs) MigrationSink(live bool, container container, snapshots []*Snapshot, conn *websocket.Conn, srcIdmap *idmap.IdmapSet, op *operation, containerOnly bool) error {
	poolName := s.getOnDiskPoolName()
	zfsRecv := func(zfsName string, writeWrapper func(io.WriteCloser) io.WriteCloser) error {
		zfsFsName := fmt.Sprintf("%s/%s", poolName, zfsName)
		args := []string{"receive", "-F", "-u", zfsFsName}
		cmd := exec.Command("zfs", args...)

		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}

		stderr, err := cmd.StderrPipe()
		if err != nil {
			return err
		}

		if err := cmd.Start(); err != nil {
			return err
		}

		writePipe := io.WriteCloser(stdin)
		if writeWrapper != nil {
			writePipe = writeWrapper(stdin)
		}

		<-shared.WebsocketRecvStream(writePipe, conn)

		output, err := ioutil.ReadAll(stderr)
		if err != nil {
			logger.Debugf("problem reading zfs recv stderr %s.", err)
		}

		err = cmd.Wait()
		if err != nil {
			logger.Errorf("problem with zfs recv: %s.", string(output))
		}
		return err
	}

	/* In some versions of zfs we can write `zfs recv -F` to mounted
	 * filesystems, and in some versions we can't. So, let's always unmount
	 * this fs (it's empty anyway) before we zfs recv. N.B. that `zfs recv`
	 * of a snapshot also needs tha actual fs that it has snapshotted
	 * unmounted, so we do this before receiving anything.
	 */
	zfsName := fmt.Sprintf("containers/%s", container.Name())
	containerMntPoint := getContainerMountPoint(s.pool.Name, container.Name())
	if shared.IsMountPoint(containerMntPoint) {
		err := zfsUmount(poolName, zfsName, containerMntPoint)
		if err != nil {
			return err
		}
	}

	if len(snapshots) > 0 {
		snapshotMntPointSymlinkTarget := shared.VarPath("storage-pools", s.pool.Name, "snapshots", s.volume.Name)
		snapshotMntPointSymlink := shared.VarPath("snapshots", container.Name())
		if !shared.PathExists(snapshotMntPointSymlink) {
			err := os.Symlink(snapshotMntPointSymlinkTarget, snapshotMntPointSymlink)
			if err != nil {
				return err
			}
		}
	}

	// At this point we have already figured out the parent
	// container's root disk device so we can simply
	// retrieve it from the expanded devices.
	parentStoragePool := ""
	parentExpandedDevices := container.ExpandedDevices()
	parentLocalRootDiskDeviceKey, parentLocalRootDiskDevice, _ := containerGetRootDiskDevice(parentExpandedDevices)
	if parentLocalRootDiskDeviceKey != "" {
		parentStoragePool = parentLocalRootDiskDevice["pool"]
	}

	// A little neuroticism.
	if parentStoragePool == "" {
		return fmt.Errorf("detected that the container's root device is missing the pool property during BTRFS migration")
	}

	for _, snap := range snapshots {
		args := snapshotProtobufToContainerArgs(container.Name(), snap)

		// Ensure that snapshot and parent container have the
		// same storage pool in their local root disk device.
		// If the root disk device for the snapshot comes from a
		// profile on the new instance as well we don't need to
		// do anything.
		if args.Devices != nil {
			snapLocalRootDiskDeviceKey, _, _ := containerGetRootDiskDevice(args.Devices)
			if snapLocalRootDiskDeviceKey != "" {
				args.Devices[snapLocalRootDiskDeviceKey]["pool"] = parentStoragePool
			}
		}
		_, err := containerCreateEmptySnapshot(container.StateObject(), args)
		if err != nil {
			return err
		}

		wrapper := StorageProgressWriter(op, "fs_progress", snap.GetName())
		name := fmt.Sprintf("containers/%s@snapshot-%s", container.Name(), snap.GetName())
		if err := zfsRecv(name, wrapper); err != nil {
			return err
		}

		snapshotMntPoint := getSnapshotMountPoint(poolName, fmt.Sprintf("%s/%s", container.Name(), *snap.Name))
		if !shared.PathExists(snapshotMntPoint) {
			err := os.MkdirAll(snapshotMntPoint, 0700)
			if err != nil {
				return err
			}
		}
	}

	defer func() {
		/* clean up our migration-send snapshots that we got from recv. */
		zfsSnapshots, err := zfsPoolListSnapshots(poolName, fmt.Sprintf("containers/%s", container.Name()))
		if err != nil {
			logger.Errorf("failed listing snapshots post migration: %s.", err)
			return
		}

		for _, snap := range zfsSnapshots {
			// If we received a bunch of snapshots, remove the migration-send-* ones, if not, wipe any snapshot we got
			if snapshots != nil && len(snapshots) > 0 && !strings.HasPrefix(snap, "migration-send") {
				continue
			}

			zfsPoolVolumeSnapshotDestroy(poolName, fmt.Sprintf("containers/%s", container.Name()), snap)
		}
	}()

	/* finally, do the real container */
	wrapper := StorageProgressWriter(op, "fs_progress", container.Name())
	if err := zfsRecv(zfsName, wrapper); err != nil {
		return err
	}

	if live {
		/* and again for the post-running snapshot if this was a live migration */
		wrapper := StorageProgressWriter(op, "fs_progress", container.Name())
		if err := zfsRecv(zfsName, wrapper); err != nil {
			return err
		}
	}

	/* Sometimes, zfs recv mounts this anyway, even if we pass -u
	 * (https://forums.freebsd.org/threads/zfs-receive-u-shouldnt-mount-received-filesystem-right.36844/)
	 * but sometimes it doesn't. Let's try to mount, but not complain about
	 * failure.
	 */
	zfsMount(poolName, zfsName)
	return nil
}

func (s *storageZfs) StorageEntitySetQuota(volumeType int, size int64, data interface{}) error {
	logger.Debugf(`Setting ZFS quota for "%s"`, s.volume.Name)

	if !shared.IntInSlice(volumeType, supportedVolumeTypes) {
		return fmt.Errorf("Invalid storage type")
	}

	var c container
	var fs string
	switch volumeType {
	case storagePoolVolumeTypeContainer:
		c = data.(container)
		fs = fmt.Sprintf("containers/%s", c.Name())
	case storagePoolVolumeTypeCustom:
		fs = fmt.Sprintf("custom/%s", s.volume.Name)
	}

	property := "quota"

	if s.pool.Config["volume.zfs.use_refquota"] != "" {
		zfsUseRefquota = s.pool.Config["volume.zfs.use_refquota"]
	}
	if s.volume.Config["zfs.use_refquota"] != "" {
		zfsUseRefquota = s.volume.Config["zfs.use_refquota"]
	}

	if shared.IsTrue(zfsUseRefquota) {
		property = "refquota"
	}

	poolName := s.getOnDiskPoolName()
	var err error
	if size > 0 {
		err = zfsPoolVolumeSet(poolName, fs, property, fmt.Sprintf("%d", size))
	} else {
		err = zfsPoolVolumeSet(poolName, fs, property, "none")
	}

	if err != nil {
		return err
	}

	logger.Debugf(`Set ZFS quota for "%s"`, s.volume.Name)
	return nil
}
