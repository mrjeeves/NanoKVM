package mesh

import "time"

// clockEpochYear is the earliest year a set clock can plausibly read. The KVM
// has no RTC: it boots at 1970 and stays there until NTP lands, which can be
// never on a device with no route out. Anything before this is not "a bit off",
// it's "never set", and code that measures elapsed wall time has to say so
// rather than compute a confident nonsense.
const clockEpochYear = 2020

// clockSane reports whether `t` looks like a wall clock that has actually been
// set. Used by the presence stamp (an unset clock is sent as "no sample" rather
// than as decades of skew) and by the CEC grant deadlines, which are absolute
// and so are only meaningful once this is true.
func clockSane(t time.Time) bool {
	return t.Year() >= clockEpochYear
}
