package guest

import (
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDirectLoopRejectsFallbackAndWrongBacking(t *testing.T) {
	backing := unix.Stat_t{Dev: 254, Ino: 1234}
	valid := unix.LoopInfo64{
		Device: uint64(backing.Dev), Inode: backing.Ino,
		Flags: unix.LO_FLAGS_DIRECT_IO | unix.LO_FLAGS_AUTOCLEAR,
	}
	if err := verifyDirectLoop(&valid, &backing); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*unix.LoopInfo64){
		func(info *unix.LoopInfo64) { info.Flags &^= unix.LO_FLAGS_DIRECT_IO },
		func(info *unix.LoopInfo64) { info.Flags &^= unix.LO_FLAGS_AUTOCLEAR },
		func(info *unix.LoopInfo64) { info.Flags |= unix.LO_FLAGS_READ_ONLY },
		func(info *unix.LoopInfo64) { info.Offset = 4096 },
		func(info *unix.LoopInfo64) { info.Sizelimit = 4096 },
		func(info *unix.LoopInfo64) { info.Inode++ },
		func(info *unix.LoopInfo64) { info.Device++ },
	} {
		info := valid
		change(&info)
		if err := verifyDirectLoop(&info, &backing); err == nil {
			t.Fatalf("accepted unsupported or mismatched loop configuration: %+v", info)
		}
	}
	fallback := valid
	fallback.Flags &^= unix.LO_FLAGS_DIRECT_IO
	if err := verifyDirectLoop(&fallback, &backing); err == nil || !strings.Contains(err.Error(), "direct I/O") {
		t.Fatalf("silent fallback needs an explicit direct-I/O error: %v", err)
	}
}
