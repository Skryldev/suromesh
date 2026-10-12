package client_test

import (
	"github.com/Skryldev/suromesh/internal/wstest"
)

func wstestNew() (*wstest.Server, error) { return wstest.New() }

func i64p(v int64) *int64   { return &v }
func strp(s string) *string { return &s }
