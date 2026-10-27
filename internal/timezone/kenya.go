package timezone

import (
	"time"
	_ "time/tzdata"
)

var Kenya = mustLoad("Africa/Nairobi")

func mustLoad(name string) *time.Location {
	location, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return location
}
