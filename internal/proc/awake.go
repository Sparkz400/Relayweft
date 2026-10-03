package proc

import "sync"

// KeepAwake keeps the machine from going to sleep (the display may still
// turn off) until the returned release function is called: on Windows with
// SetThreadExecutionState(ES_CONTINUOUS|ES_SYSTEM_REQUIRED), on macOS with
// a `caffeinate -i` child, elsewhere not at all. Calls nest: the machine
// may sleep again once every caller released. release is idempotent.
func KeepAwake() (release func()) {
	awakeMu.Lock()
	defer awakeMu.Unlock()
	awakeRefs++
	if awakeRefs == 1 {
		awakeStop = startAwake()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			awakeMu.Lock()
			defer awakeMu.Unlock()
			awakeRefs--
			if awakeRefs == 0 && awakeStop != nil {
				awakeStop()
				awakeStop = nil
			}
		})
	}
}

var (
	awakeMu   sync.Mutex
	awakeRefs int
	awakeStop func()
)

// Awake reports whether a KeepAwake is in effect (tests, status lines).
func Awake() bool {
	awakeMu.Lock()
	defer awakeMu.Unlock()
	return awakeRefs > 0
}
