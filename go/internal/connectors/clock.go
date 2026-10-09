package connectors

import (
	"strings"
	"time"
)

// timeNow is replaceable so tests need not depend on today's date.
var timeNow = time.Now

func timeParseDay(v string) (time.Time, error) { return time.Parse("2006-01-02", strings.TrimSpace(v)) }
