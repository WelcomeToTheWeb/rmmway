package session

import "time"

// nowMS is the shared wall-clock source for frame timestamps.
func nowMS() int64 { return time.Now().UnixMilli() }
