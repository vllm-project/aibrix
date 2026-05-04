/*
Copyright 2025 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func makeHS256JWT(t *testing.T, secret []byte, payload string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	signed := header + "." + body
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signed))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signed + "." + sig
}

func TestHMACKeySet(t *testing.T) {
	secret := []byte("client-secret-xyz")
	ks := &hmacKeySet{secret: secret}

	t.Run("valid signature", func(t *testing.T) {
		jwt := makeHS256JWT(t, secret, `{"sub":"u1"}`)
		payload, err := ks.VerifySignature(context.Background(), jwt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(string(payload), `"sub":"u1"`) {
			t.Fatalf("unexpected payload: %s", payload)
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		jwt := makeHS256JWT(t, []byte("other-secret"), `{"sub":"u1"}`)
		if _, err := ks.VerifySignature(context.Background(), jwt); err == nil {
			t.Fatal("expected signature mismatch, got nil")
		}
	})

	t.Run("malformed jwt", func(t *testing.T) {
		if _, err := ks.VerifySignature(context.Background(), "not.a.jwt.at.all"); err == nil {
			t.Fatal("expected error")
		}
		if _, err := ks.VerifySignature(context.Background(), "abc"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("tampered payload", func(t *testing.T) {
		jwt := makeHS256JWT(t, secret, `{"sub":"u1"}`)
		parts := strings.Split(jwt, ".")
		parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"attacker"}`))
		if _, err := ks.VerifySignature(context.Background(), strings.Join(parts, ".")); err == nil {
			t.Fatal("expected signature mismatch on tampered payload")
		}
	})
}

func TestRoleFromClaims(t *testing.T) {
	a := &AuthMiddleware{
		config:          AuthConfig{OIDCGroupsClaim: "groups"},
		oidcAdminGroups: parseAdminGroups("aibrix-admin, platform-ops"),
	}

	cases := []struct {
		name   string
		claims map[string]interface{}
		want   string
	}{
		{"no claim", map[string]interface{}{}, roleViewer},
		{"empty list", map[string]interface{}{"groups": []interface{}{}}, roleViewer},
		{"non-admin group", map[string]interface{}{"groups": []interface{}{"users"}}, roleViewer},
		{"single string admin", map[string]interface{}{"groups": "aibrix-admin"}, roleAdmin},
		{"array string admin", map[string]interface{}{"groups": []string{"users", "platform-ops"}}, roleAdmin},
		{"array iface admin", map[string]interface{}{"groups": []interface{}{"users", "aibrix-admin"}}, roleAdmin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.roleFromClaims(tc.claims); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}

	// With no admin groups configured, everyone is viewer regardless.
	noAdmins := &AuthMiddleware{config: AuthConfig{OIDCGroupsClaim: "groups"}}
	if got := noAdmins.roleFromClaims(map[string]interface{}{"groups": []interface{}{"aibrix-admin"}}); got != roleViewer {
		t.Fatalf("got %q want %q", got, roleViewer)
	}
}

func TestRoleFromClaimsCustomClaimName(t *testing.T) {
	a := &AuthMiddleware{
		config:          AuthConfig{OIDCGroupsClaim: "roles"},
		oidcAdminGroups: parseAdminGroups("admin"),
	}
	// "groups" should be ignored; "roles" should be consulted.
	if got := a.roleFromClaims(map[string]interface{}{"groups": []interface{}{"admin"}}); got != roleViewer {
		t.Fatalf("got %q want %q", got, roleViewer)
	}
	if got := a.roleFromClaims(map[string]interface{}{"roles": []interface{}{"admin"}}); got != roleAdmin {
		t.Fatalf("got %q want %q", got, roleAdmin)
	}
}
