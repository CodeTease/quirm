package main

import (
	"encoding/json"
	"fmt"
)

func detailsToString(m map[string]string) string {
	b, _ := json.Marshal(m)
	return string(b)
}
