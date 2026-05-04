package sab

import "time"

func strPtr(s string) *string  { return &s }
func timePtrNow() *time.Time   { t := time.Now().UTC(); return &t }
