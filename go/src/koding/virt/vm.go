package virt

import (
	"fmt"
	"io"
	"io/ioutil"
	"koding/virt/models"
	"labix.org/v2/mgo/bson"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"text/template"
	"time"
)

type VM models.VM
type Permissions models.Permissions

var templateDir string
var Templates = template.New("lxc")

func LoadTemplates(dir string) error {
	interf, err := net.InterfaceByName("lxcbr0")
	if err != nil {
		return err
	}
	addrs, err := interf.Addrs()
	if err != nil {
		return err
	}
	hostIP, _, err := net.ParseCIDR(addrs[0].String())
	if err != nil {
		return err
	}

	templateDir = dir
	Templates.Funcs(template.FuncMap{
		"hostIP": func() string { return hostIP.String() },
	})
	if _, err := Templates.ParseGlob(templateDir + "/vm/*"); err != nil {
		return err
	}

	return nil
}

func VMName(vmId bson.ObjectId) string {
	return "vm-" + vmId.Hex()
}

func (vm *VM) String() string {
	return VMName(vm.Id)
}

func (vm *VM) VEth() string {
	return fmt.Sprintf("veth-%x", []byte(vm.IP[12:16]))
}

func (vm *VM) MAC() net.HardwareAddr {
	return net.HardwareAddr([]byte{0, 0, vm.IP[12], vm.IP[13], vm.IP[14], vm.IP[15]})
}

func (vm *VM) RbdDevice() string {
	return "/dev/rbd/vms/" + vm.String()
}

func (vm *VM) File(p string) string {
	return fmt.Sprintf("/var/lib/lxc/%s/%s", vm, p)
}

func (vm *VM) OverlayFile(p string) string {
	return vm.File("overlay/" + p)
}

func (vm *VM) LowerdirFile(p string) string {
	return vm.VMRoot + "rootfs/" + p
}

func (vm *VM) PtsDir() string {
	return vm.File("rootfs/dev/pts")
}

func (vm *VM) GetPermissions(user *User) *Permissions {
	for _, entry := range vm.Users {
		if entry.Id == user.ObjectId {
			p := Permissions(entry)
			return &p
		}
	}
	return nil
}

func (vm *VM) Prepare(reinitialize bool) {
	vm.Unprepare()

	var err error
	vm.VMRoot, err = os.Readlink("/var/lib/lxc/vmroot/")
	if err != nil {
		vm.VMRoot = "/var/lib/lxc/vmroot/"
	}
	if vm.VMRoot[0] != '/' {
		vm.VMRoot = "/var/lib/lxc/" + vm.VMRoot
	}

	// write LXC files
	prepareDir(vm.File(""), 0)
	vm.generateFile(vm.File("config"), "config", 0, false)
	vm.generateFile(vm.File("fstab"), "fstab", 0, false)
	vm.generateFile(vm.File("ip-address"), "ip-address", 0, false)

	// map rbd image to block device
	if err := vm.MountRBD(vm.OverlayFile("")); err != nil {
		panic(err)
	}

	// remove all except /home on reinitialize
	if reinitialize {
		entries, err := ioutil.ReadDir(vm.OverlayFile("/"))
		if err != nil {
			panic(err)
		}
		for _, entry := range entries {
			if entry.Name() != "home" {
				os.RemoveAll(vm.OverlayFile("/" + entry.Name()))
			}
		}
	}

	// prepare overlay
	prepareDir(vm.OverlayFile("/"), RootIdOffset)           // for chown
	prepareDir(vm.OverlayFile("/lost+found"), RootIdOffset) // for chown
	prepareDir(vm.OverlayFile("/etc"), RootIdOffset)
	vm.generateFile(vm.OverlayFile("/etc/hostname"), "hostname", RootIdOffset, false)
	vm.generateFile(vm.OverlayFile("/etc/hosts"), "hosts", RootIdOffset, false)
	vm.generateFile(vm.OverlayFile("/etc/ldap.conf"), "ldap.conf", RootIdOffset, false)
	vm.MergePasswdFile()
	vm.MergeGroupFile()
	vm.MergeDpkgDatabase()

	// mount overlay
	prepareDir(vm.File("rootfs"), RootIdOffset)
	// if out, err := exec.Command("/bin/mount", "--no-mtab", "-t", "overlayfs", "-o", fmt.Sprintf("lowerdir=%s,upperdir=%s", vm.LowerdirFile("/"), vm.OverlayFile("/")), "overlayfs", vm.File("rootfs")).CombinedOutput(); err != nil {
	if out, err := exec.Command("/bin/mount", "--no-mtab", "-t", "aufs", "-o", fmt.Sprintf("noplink,br=%s:%s", vm.OverlayFile("/"), vm.LowerdirFile("/")), "aufs", vm.File("rootfs")).CombinedOutput(); err != nil {
		panic(commandError("mount overlay failed.", err, out))
	}

	// mount devpts
	prepareDir(vm.PtsDir(), RootIdOffset)
	if out, err := exec.Command("/bin/mount", "--no-mtab", "-t", "devpts", "-o", "rw,noexec,nosuid,newinstance,gid="+strconv.Itoa(RootIdOffset+5)+",mode=0620,ptmxmode=0666", "devpts", vm.PtsDir()).CombinedOutput(); err != nil {
		panic(commandError("mount devpts failed.", err, out))
	}
	chown(vm.PtsDir(), RootIdOffset, RootIdOffset)
	chown(vm.PtsDir()+"/ptmx", RootIdOffset, RootIdOffset)

	// add ebtables entry to restrict IP and MAC
	if out, err := exec.Command("/sbin/ebtables", "--append", "VMS", "--protocol", "IPv4", "--source", vm.MAC().String(), "--ip-src", vm.IP.String(), "--in-interface", vm.VEth(), "--jump", "ACCEPT").CombinedOutput(); err != nil {
		panic(commandError("ebtables rule addition failed.", err, out))
	}

	// add a static route so it is redistributed by BGP
	if out, err := exec.Command("/sbin/route", "add", vm.IP.String(), "lxcbr0").CombinedOutput(); err != nil {
		panic(commandError("adding route failed.", err, out))
	}
}

func UnprepareVM(id bson.ObjectId) error {
	vm := VM{Id: id}
	return vm.Unprepare()
}

func (vm *VM) Unprepare() error {
	var firstError error

	// stop VM
	if err := vm.Shutdown(); err != nil {
		panic(err)
	}

	// backup dpkg database for statistical purposes
	os.Mkdir("/var/lib/lxc/dpkg-statuses", 0755)
	copyFile(vm.OverlayFile("/var/lib/dpkg/status"), "/var/lib/lxc/dpkg-statuses/"+vm.String(), RootIdOffset)

	if vm.IP == nil {
		if ip, err := ioutil.ReadFile(vm.File("ip-address")); err == nil {
			vm.IP = net.ParseIP(string(ip))
		}
	}

	if vm.IP != nil {
		// remove ebtables entry
		if out, err := exec.Command("/sbin/ebtables", "--delete", "VMS", "--protocol", "IPv4", "--source", vm.MAC().String(), "--ip-src", vm.IP.String(), "--in-interface", vm.VEth(), "--jump", "ACCEPT").CombinedOutput(); err != nil && firstError == nil {
			firstError = commandError("ebtables rule deletion failed.", err, out)
		}

		// remove the static route so it is no longer redistribed by BGP
		if out, err := exec.Command("/sbin/route", "del", vm.IP.String(), "lxcbr0").CombinedOutput(); err != nil {
			firstError = commandError("Removing route failed.", err, out)
		}
	}

	// unmount and unmap everything
	if out, err := exec.Command("/bin/umount", vm.PtsDir()).CombinedOutput(); err != nil && firstError == nil {
		firstError = commandError("umount devpts failed.", err, out)
	}
	//Flush the aufs
	if out, err := exec.Command("/sbin/auplink", vm.File("rootfs"), "flush").CombinedOutput(); err != nil && firstError == nil {
		firstError = commandError("AUFS flush failed.", err, out)
	}
	if out, err := exec.Command("/bin/umount", vm.File("rootfs")).CombinedOutput(); err != nil && firstError == nil {
		firstError = commandError("umount overlay failed.", err, out)
	}
	if err := vm.UnmountRBD(vm.OverlayFile("")); err != nil && firstError == nil {
		firstError = err
	}

	// remove VM directory
	os.Remove(vm.File("config"))
	os.Remove(vm.File("fstab"))
	os.Remove(vm.File("ip-address"))
	os.Remove(vm.File("rootfs"))
	os.Remove(vm.File("rootfs.hold"))
	os.Remove(vm.File(""))

	return firstError
}

func (vm *VM) MountRBD(mountDir string) error {
	makeFileSystem := false

	// create image if it does not exist
	if out, err := exec.Command("/usr/bin/rbd", "info", "--pool", "vms", "--image", vm.String()).CombinedOutput(); err != nil {
		exitError, isExitError := err.(*exec.ExitError)
		if !isExitError || exitError.Sys().(syscall.WaitStatus).ExitStatus() != 1 {
			return commandError("rbd info failed.", err, out)
		}

		if vm.DiskSizeInMB == 0 {
			vm.DiskSizeInMB = 1200
		}
		if vm.SnapshotName == "" {
			if out, err := exec.Command("/usr/bin/rbd", "create", "--pool", "vms", "--size", strconv.Itoa(vm.DiskSizeInMB), "--image", vm.String(), "--image-format", "1").CombinedOutput(); err != nil {
				return commandError("rbd create failed.", err, out)
			}
		}
		if vm.SnapshotName != "" {
			if out, err := exec.Command("/usr/bin/rbd", "clone", "--pool", "vms", "--image", VMName(vm.SnapshotVM), "--snap", vm.SnapshotName, "--dest-pool", "vms", "--dest", vm.String()).CombinedOutput(); err != nil {
				return commandError("rbd clone failed.", err, out)
			}
		}

		makeFileSystem = true
	}

	// map image
	if out, err := exec.Command("/usr/bin/rbd", "map", "--pool", "vms", "--image", vm.String()).CombinedOutput(); err != nil {
		return commandError("rbd map failed.", err, out)
	}

	// wait for rbd device to appear
	for {
		_, err := os.Stat(vm.RbdDevice())
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		time.Sleep(time.Second / 2)
	}

	if makeFileSystem {
		if out, err := exec.Command("/sbin/mkfs.ext4", vm.RbdDevice()).CombinedOutput(); err != nil {
			return commandError("mkfs.ext4 failed.", err, out)
		}
	}

	// check/correct filesystem
	if out, err := exec.Command("/sbin/fsck.ext4", "-p", vm.RbdDevice()).CombinedOutput(); err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok || exitError.Sys().(syscall.WaitStatus).ExitStatus() == 4 {
			if out, err := exec.Command("/sbin/fsck.ext4", "-y", vm.RbdDevice()).CombinedOutput(); err != nil {
				exitError, ok := err.(*exec.ExitError)
				if !ok || exitError.Sys().(syscall.WaitStatus).ExitStatus() != 1 {
					return commandError(fmt.Sprintf("fsck.ext4 could not automatically repair FS for %s.", vm.HostnameAlias), err, out)
				}
			}
		} else {
			return commandError(fmt.Sprintf("fsck.ext4 failed %s.", vm.HostnameAlias), err, out)
		}
	}

	if err := os.Mkdir(mountDir, 0755); err != nil && !os.IsExist(err) {
		return err
	}
	if out, err := exec.Command("/bin/mount", "-t", "ext4", vm.RbdDevice(), mountDir).CombinedOutput(); err != nil {
		os.Remove(mountDir)
		return commandError("mount rbd failed.", err, out)
	}

	return nil
}

func (vm *VM) UnmountRBD(mountDir string) error {
	var firstError error
	if out, err := exec.Command("/bin/umount", vm.OverlayFile("")).CombinedOutput(); err != nil && firstError == nil {
		firstError = commandError("umount rbd failed.", err, out)
	}
	if out, err := exec.Command("/usr/bin/rbd", "unmap", vm.RbdDevice()).CombinedOutput(); err != nil && firstError == nil {
		firstError = commandError("rbd unmap failed.", err, out)
	}
	os.Remove(mountDir)
	return firstError
}

func (vm *VM) ResizeRBD() error {
	if out, err := exec.Command("/usr/bin/rbd", "resize", "--pool", "vms", "--image", vm.String(), "--size", strconv.Itoa(vm.DiskSizeInMB)).CombinedOutput(); err != nil {
		return commandError("rbd resize failed.", err, out)
	}

	if out, err := exec.Command("/sbin/resize2fs", vm.RbdDevice()).CombinedOutput(); err != nil {
		return commandError("resize2fs failed.", err, out)
	}

	if out, err := exec.Command("/bin/mount", "-o", "remount", vm.OverlayFile("")).CombinedOutput(); err != nil {
		return commandError("remount failed.", err, out)
	}

	return nil
}

const FIFREEZE = 0xC0045877
const FITHAW = 0xC0045878

func (vm *VM) FreezeFileSystem() error {
	return vm.controlOverlay(FIFREEZE)
}

func (vm *VM) ThawFileSystem() error {
	return vm.controlOverlay(FITHAW)
}

func (vm *VM) controlOverlay(action uintptr) error {
	fd, err := os.Open(vm.OverlayFile(""))
	if err != nil {
		return err
	}
	defer fd.Close()
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd.Fd(), action, 0); errno != 0 {
		return errno
	}
	return nil
}

func (vm *VM) CreateConsistentSnapshot(snapshotName string) error {
	if err := vm.FreezeFileSystem(); err != nil {
		return err
	}
	defer vm.ThawFileSystem()
	if out, err := exec.Command("/usr/bin/rbd", "snap", "create", "--pool", "vms", "--image", vm.String(), "--snap", snapshotName).CombinedOutput(); err != nil {
		return commandError("Creating snapshot failed.", err, out)
	}
	if out, err := exec.Command("/usr/bin/rbd", "snap", "protect", "--pool", "vms", "--image", vm.String(), "--snap", snapshotName).CombinedOutput(); err != nil {
		return commandError("Protecting snapshot failed.", err, out)
	}
	return nil
}

func (vm *VM) DeleteSnapshot(snapshotName string) error {
	if out, err := exec.Command("/usr/bin/rbd", "snap", "unprotect", "--pool", "vms", "--image", vm.String(), "--snap", snapshotName).CombinedOutput(); err != nil {
		return commandError("Unprotecting snapshot failed.", err, out)
	}
	if out, err := exec.Command("/usr/bin/rbd", "snap", "rm", "--pool", "vms", "--image", vm.String(), "--snap", snapshotName).CombinedOutput(); err != nil {
		return commandError("Removing snapshot failed.", err, out)
	}
	return nil
}

func DestroyVM(id bson.ObjectId) error {
	if out, err := exec.Command("/usr/bin/rbd", "rm", "--pool", "vms", "--image", VMName(id)).CombinedOutput(); err != nil {
		return commandError("Removing image failed.", err, out)
	}
	return nil
}

func commandError(message string, err error, out []byte) error {
	return fmt.Errorf("%s\n%s\n%s", message, err.Error(), string(out))
}

// may panic
func prepareDir(p string, id int) {
	if err := os.Mkdir(p, 0755); err != nil && !os.IsExist(err) {
		panic(err)
	}
	chown(p, id, id)
}

// may panic
func (vm *VM) generateFile(p, template string, id int, executable bool) {
	var mode os.FileMode = 0644
	if executable {
		mode = 0755
	}
	file, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	if err := Templates.ExecuteTemplate(file, template, vm); err != nil {
		panic(err)
	}

	if err := file.Chown(id, id); err != nil {
		panic(err)
	}
	if err := file.Chmod(mode); err != nil {
		panic(err)
	}
}

// may panic
func chown(p string, uid, gid int) {
	if err := os.Chown(p, uid, gid); err != nil {
		panic(err)
	}
}

func copyFile(src, dst string, id int) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()

	fi, err := sf.Stat()
	if err != nil {
		return err
	}

	df, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode())
	if err != nil {
		return err
	}
	defer df.Close()

	if _, err := io.Copy(df, sf); err != nil {
		return err
	}

	if err := df.Chown(id, id); err != nil {
		return err
	}

	return nil
}
