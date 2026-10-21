package webapp

import "time"

// SetDemoGrace : délai entre échéance et arrêt d'une démo (tests).
func SetDemoGrace(d time.Duration) { demoGrace = d }
