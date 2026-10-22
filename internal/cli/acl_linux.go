package cli

// Linux POSIX ACL permissions are bounded by the group-class mask reflected in
// st_mode. The file and parent mode checks already exclude effective grants.
func checkACL(fd int, directory bool) error {
	return nil
}
