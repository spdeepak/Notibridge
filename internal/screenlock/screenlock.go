// Package screenlock reports whether the macOS screen is locked.
//
// The state comes from the WindowServer session dictionary, which carries
// "CGSSessionScreenIsLocked" only while the screen is locked. The lookup is
// done in C so the CoreFoundation pointer juggling stays out of Go.
package screenlock

/*
#cgo LDFLAGS: -framework ApplicationServices
#include <ApplicationServices/ApplicationServices.h>

// nb_locked returns 1 while the screen is locked, 0 otherwise. A session
// dictionary that cannot be read counts as unlocked.
static int nb_locked(void) {
	CFDictionaryRef dict = CGSessionCopyCurrentDictionary();
	if (dict == NULL) {
		return 0;
	}
	CFStringRef key = CFStringCreateWithCString(NULL, "CGSSessionScreenIsLocked", kCFStringEncodingUTF8);
	int locked = 0;
	if (key != NULL) {
		CFTypeRef value = CFDictionaryGetValue(dict, key);
		if (value != NULL && CFGetTypeID(value) == CFBooleanGetTypeID()) {
			locked = CFBooleanGetValue((CFBooleanRef)value) ? 1 : 0;
		}
		CFRelease(key);
	}
	CFRelease(dict);
	return locked;
}
*/
import "C"

// State reports the screen lock state of the running session.
type State interface {
	// Locked reports whether the screen is currently locked.
	Locked() bool
}

// MacOS returns the live macOS screen lock state.
func MacOS() State { return macOSState{} }

type macOSState struct{}

// Locked implements State.
func (macOSState) Locked() bool { return C.nb_locked() != 0 }
