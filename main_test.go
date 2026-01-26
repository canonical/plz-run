// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Massimiliano Girardi

package main

import (
	"testing"
)

func TestParseSystemdVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    uint64
		wantErr bool
	}{
		{
			name:    "simple version",
			version: "256",
			want:    256,
			wantErr: false,
		},
		{
			name:    "empty string",
			version: "",
			want:    0,
			wantErr: true,
		},
		{
			name:    "arch string",
			version: "259-1-arch",
			want:    259,
			wantErr: false,
		},
		{
			name:    "ubuntu string",
			version: "255.4-1ubuntu8.8",
			want:    255,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSystemdVersion(tt.version)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseSystemdVersion() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("parseSystemdVersion() = %v, want %v", got, tt.want)
			}
		})
	}
}
