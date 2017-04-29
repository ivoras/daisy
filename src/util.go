package main

import "time"

func unixTimeStampToUTCTime(ts int) time.Time {
	return time.Unix(int64(ts), 0)
}
