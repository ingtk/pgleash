package cli

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin ACL grants can bypass st_mode. Read extended security metadata on the
// same descriptor we checked, using the ABI in sys/attr.h and sys/kauth.h.
func checkACL(fd int, directory bool) error {
	attributes := [6]uint32{5, unix.ATTR_CMN_EXTENDED_SECURITY}
	var buffer [8192]byte // More than the platform's maximum extended ACL size.
	_, _, errno := syscall.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd),
		uintptr(unsafe.Pointer(&attributes[0])), uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)), 0, 0)
	runtime.KeepAlive(&attributes)
	runtime.KeepAlive(&buffer)
	if errno != 0 {
		return fmt.Errorf("cannot inspect configuration ACL")
	}
	order := binary.LittleEndian // Both supported Darwin architectures are LE.
	total := int64(order.Uint32(buffer[0:4]))
	start := 4 + int64(int32(order.Uint32(buffer[4:8])))
	length := int64(order.Uint32(buffer[8:12]))
	if total < 12 || total > int64(len(buffer)) || start < 12 || start+length > total {
		return fmt.Errorf("invalid configuration ACL metadata")
	}
	if length == 0 {
		return nil
	}
	security := buffer[start : start+length]
	// kauth_filesec: magic + owner/group GUIDs + ACL count/flags, then ACEs.
	const headerSize, entrySize = 44, 24
	if len(security) < headerSize || order.Uint32(security[:4]) != 0x012cc16d {
		return fmt.Errorf("invalid configuration ACL metadata")
	}
	count := order.Uint32(security[36:40])
	if count == 0xffffffff { // KAUTH_FILESEC_NOACL
		return nil
	}
	if uint64(count)*entrySize+headerSize != uint64(len(security)) {
		return fmt.Errorf("invalid configuration ACL metadata")
	}
	// Conservatively refuse mutation grants to any principal, without needing
	// group/UUID resolution. Deny entries (such as a home directory's default
	// everyone-deny-delete ACL) do not broaden access and remain supported.
	const mutationRights = 1<<2 | 1<<4 | 1<<5 | 1<<6 | 1<<8 | 1<<10 | 1<<12 | 1<<13 | 1<<21 | 1<<23
	const readRights = 1<<1 | 1<<24
	for i := uint32(0); i < count; i++ {
		entry := security[headerSize+int(i)*entrySize:]
		kind := order.Uint32(entry[16:20]) & 0xf
		rights := order.Uint32(entry[20:24])
		if kind == 2 { // KAUTH_ACE_DENY
			continue
		}
		if kind != 1 || rights&mutationRights != 0 || (!directory && rights&readRights != 0) {
			return fmt.Errorf("configuration ACL grants unsafe access")
		}
	}
	return nil
}
