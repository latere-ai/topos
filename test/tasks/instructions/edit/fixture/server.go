// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package servers builds the HTTP servers of the shop.
package servers

import (
	"net/http"
	"time"
)

// NewServer is the public server.
func NewServer(addr string) *http.Server {
	return &http.Server{
		Addr:         addr,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
}

// NewAdminServer is the server of the operators' console.
func NewAdminServer(addr string) *http.Server {
	return &http.Server{
		Addr:         addr,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
}
