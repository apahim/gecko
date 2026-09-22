package main

import (
	"strings"
	"testing"
)

func TestValidatePublicAuthAddress(t *testing.T) {
	tests := []struct {
		name          string
		enablePublic  bool
		address       string
		publicAddress string
		devAuth       bool
		disableAuth   bool
		wantErr       bool
	}{
		{
			name:         "auth enabled does not require loopback",
			enablePublic: true,
			address:      "0.0.0.0",
		},
		{
			name:          "dev auth loopback",
			enablePublic:  true,
			publicAddress: "127.0.0.1",
			devAuth:       true,
		},
		{
			name:          "dev auth localhost",
			enablePublic:  true,
			publicAddress: "localhost",
			devAuth:       true,
		},
		{
			name:          "dev auth IPv6 loopback",
			enablePublic:  true,
			publicAddress: "::1",
			devAuth:       true,
		},
		{
			name:          "dev auth non-loopback",
			enablePublic:  true,
			publicAddress: "0.0.0.0",
			devAuth:       true,
			wantErr:       true,
		},
		{
			name:         "dev auth implicit non-loopback",
			enablePublic: true,
			address:      "0.0.0.0",
			devAuth:      true,
			wantErr:      true,
		},
		{
			name:          "disabled auth non-loopback",
			enablePublic:  true,
			publicAddress: "192.0.2.10",
			disableAuth:   true,
			wantErr:       true,
		},
		{
			name:          "public API disabled",
			enablePublic:  false,
			publicAddress: "0.0.0.0",
			devAuth:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePublicAuthAddress(tt.enablePublic, tt.address, tt.publicAddress, tt.devAuth, tt.disableAuth)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validatePublicAuthAddress() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				for _, address := range []string{tt.address, tt.publicAddress} {
					if address != "" && strings.Contains(err.Error(), address) {
						t.Errorf("validation error exposes bind address %q: %v", address, err)
					}
				}
			}
		})
	}
}
