package mockphone

import "testing"

// A backlog of continuous events must leave room for on_change events, so
// it can't trigger the on_change overflow close (spec §5).
func TestContinuousEventsLeaveRoomForOnChange(t *testing.T) {
	for _, size := range []int{1, 4, 8, 1024} {
		s := &session{out: make(chan []byte, size)}
		cont := 0
		for s.trySend(nil, true) {
			cont++
		}
		onChange := 0
		for s.trySend(nil, false) {
			onChange++
		}
		if cont != continuousLimit(size) || cont+onChange != size {
			t.Errorf("queue %d: %d continuous + %d on_change slots", size, cont, onChange)
		}
		if size >= 4 && onChange < size/4 {
			t.Errorf("queue %d: only %d slots left for on_change", size, onChange)
		}
	}
}
