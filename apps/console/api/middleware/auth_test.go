/*
Copyright 2025 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package middleware

import "testing"

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
