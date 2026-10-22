//go:build darwin || linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Open each component relative to a verified directory descriptor. Neither a
// symlink swap nor a later path lookup can substitute the file we validate/read.
func openConfig(who identity) (*os.File, error) {
	if !filepath.IsAbs(who.home) || who.uid < 0 {
		return nil, fmt.Errorf("cannot resolve a trusted home directory")
	}
	const directoryFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	dir, err := unix.Open("/", directoryFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot open configuration directory")
	}
	defer func() { unix.Close(dir) }()
	if err := trustedDirectory(dir, who.uid); err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(who.home), "/"), "/")
	parts = append(parts, ".config", "pgleash")
	for _, part := range parts {
		if part == "" {
			continue
		}
		next, err := unix.Openat(dir, part, directoryFlags, 0)
		if err != nil {
			return nil, fmt.Errorf("cannot open configuration directory (symlinks are not allowed)")
		}
		unix.Close(dir)
		dir = next
		if err := trustedDirectory(dir, who.uid); err != nil {
			return nil, err
		}
	}
	// Nonblocking open prevents a substituted FIFO from hanging before fstat.
	fd, err := unix.Openat(dir, "config.toml", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot open configuration file (symlinks are not allowed)")
	}
	file := os.NewFile(uintptr(fd), "config.toml")
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, fmt.Errorf("cannot inspect configuration file")
	}
	mode := uint32(stat.Mode)
	if mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(who.uid) || (mode&07777 != 0400 && mode&07777 != 0600) {
		file.Close()
		return nil, fmt.Errorf("configuration must be an owned regular file with mode 0400 or 0600")
	}
	if err := checkACL(fd, false); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func trustedDirectory(fd, uid int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("cannot inspect configuration directory")
	}
	mode := uint32(stat.Mode)
	if mode&unix.S_IFMT != unix.S_IFDIR || (stat.Uid != 0 && stat.Uid != uint32(uid)) {
		return fmt.Errorf("configuration directory has an untrusted owner")
	}
	// Root-owned sticky directories such as /tmp protect owned child entries
	// from replacement by other users. All other writable ancestors are refused.
	if mode&0022 != 0 && !(stat.Uid == 0 && mode&unix.S_ISVTX != 0) {
		return fmt.Errorf("configuration directory permits replacement by another user")
	}
	return checkACL(fd, true)
}
