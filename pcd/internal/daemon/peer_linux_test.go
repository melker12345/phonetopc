package daemon

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
)

const procHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func TestSocketOwner(t *testing.T) {
	var probe [4]byte
	binary.NativeEndian.PutUint32(probe[:], 1)
	if probe[0] != 1 {
		t.Skip("the sample lines are from a little-endian machine")
	}
	tcp := procHeader +
		"   0: 0100007F:223F 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 11 1\n" +
		"   1: 0100007F:223F 0100007F:9C40 01 00000000:00000000 00:00000000 00000000  1000        0 12 1\n" +
		"   2: 0100007F:9C40 0100007F:223F 01 00000000:00000000 00:00000000 00000000  1001        0 13 1\n"
	tcp6 := procHeader +
		"   0: 00000000000000000000000001000000:223F 00000000000000000000000001000000:9C41 01 00000000:00000000 00:00000000 00000000  1002        0 14 1\n" +
		"   1: 0000000000000000FFFF00000100007F:9C42 0000000000000000FFFF00000100007F:223F 01 00000000:00000000 00:00000000 00000000  1003        0 15 1\n"
	ap := netip.MustParseAddrPort
	for _, tc := range []struct {
		listing       string
		local, remote netip.AddrPort
		uid           int
		found         bool
	}{
		{tcp, ap("127.0.0.1:40000"), ap("127.0.0.1:8767"), 1001, true},
		{tcp, ap("127.0.0.1:8767"), ap("127.0.0.1:40000"), 1000, true},
		{tcp, ap("127.0.0.1:40001"), ap("127.0.0.1:8767"), 0, false},
		{tcp6, ap("[::1]:8767"), ap("[::1]:40001"), 1002, true},
		{tcp6, ap("127.0.0.1:40002"), ap("127.0.0.1:8767"), 1003, true},
	} {
		uid, found := socketOwner(strings.NewReader(tc.listing), tc.local, tc.remote)
		if uid != tc.uid || found != tc.found {
			t.Errorf("%v→%v: got %d %v, want %d %v", tc.local, tc.remote, uid, found, tc.uid, tc.found)
		}
	}
}
