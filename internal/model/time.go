package model

import (
	"fmt"
	"time"
)

type LocalTime time.Time

const timeFormat = "2006-01-02 15:04:05"

func (t LocalTime) MarshalJSON() ([]byte, error) {
	formatted := fmt.Sprintf("\"%s\"", time.Time(t).Format(timeFormat))
	return []byte(formatted), nil
}
