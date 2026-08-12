/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package error_injection

import (
	"context"
	"testing"
)

// TestCheckPoint_PerJobConfig tests injection with per-job config from context.
func TestCheckPoint_PerJobConfig(t *testing.T) {
	// Register a point with template
	point := &InjectionPoint{
		ID:          "test.perjob",
		Component:   "test",
		Action:      "perjob",
		Description: "Test point for per-job config",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeTimeout: {
				Type:            ErrorTypeTimeout,
				Code:            CodeDeadlineExceeded,
				MessageTemplate: "timeout error in {{.component}}",
				Placeholders:    map[string]string{"component": "test"},
			},
		},
	}
	DefaultRegistry[point.ID] = point

	t.Run("probability 1.0 always triggers", func(t *testing.T) {
		cfg := &InjectionConfig{
			Enabled: true,
			Rules: []InjectionRule{
				{
					PointRef:    "test.perjob",
					ErrorType:   ErrorTypeTimeout,
					Probability: 1.0,
				},
			},
			JobID: "test-job-1",
		}
		ctx := WithInjectionContext(context.Background(), cfg)

		// Should always trigger with probability 1.0
		for i := 0; i < 10; i++ {
			// Create fresh injector for each iteration to get fresh random
			inj, _ := NewInjector()
			err := inj.CheckPoint(ctx, "test.perjob")
			if err == nil {
				t.Errorf("CheckPoint() iteration %d: expected error with probability 1.0, got nil", i)
			}
		}
	})

	t.Run("probability 0.0 never triggers", func(t *testing.T) {
		injector, _ := NewInjector()

		cfg := &InjectionConfig{
			Enabled: true,
			Rules: []InjectionRule{
				{
					PointRef:    "test.perjob",
					ErrorType:   ErrorTypeTimeout,
					Probability: 0.0,
				},
			},
			JobID: "test-job-2",
		}
		ctx := WithInjectionContext(context.Background(), cfg)

		// Should never trigger with probability 0.0
		for i := 0; i < 10; i++ {
			err := injector.CheckPoint(ctx, "test.perjob")
			if err != nil {
				t.Errorf("CheckPoint() iteration %d: expected nil with probability 0.0, got error: %v", i, err)
			}
		}
	})

	t.Run("proper error returned", func(t *testing.T) {
		cfg := &InjectionConfig{
			Enabled: true,
			Rules: []InjectionRule{
				{
					PointRef:    "test.perjob",
					ErrorType:   ErrorTypeTimeout,
					Probability: 1.0,
				},
			},
			JobID: "test-job-3",
		}
		ctx := WithInjectionContext(context.Background(), cfg)

		inj, _ := NewInjector()
		err := inj.CheckPoint(ctx, "test.perjob")
		if err == nil {
			t.Errorf("CheckPoint() expected error, got nil")
			return
		}

		// Verify error is an InjectedError
		injectedErr, ok := err.(*InjectedError)
		if !ok {
			t.Errorf("CheckPoint() returned non-InjectedError: %T", err)
			return
		}

		if injectedErr.Type != ErrorTypeTimeout {
			t.Errorf("InjectedError.Type = %v, want %v", injectedErr.Type, ErrorTypeTimeout)
		}
		if injectedErr.Code != CodeDeadlineExceeded {
			t.Errorf("InjectedError.Code = %v, want %v", injectedErr.Code, CodeDeadlineExceeded)
		}
	})

	t.Run("overrides applied correctly", func(t *testing.T) {
		cfg := &InjectionConfig{
			Enabled: true,
			Rules: []InjectionRule{
				{
					PointRef:    "test.perjob",
					ErrorType:   ErrorTypeTimeout,
					Probability: 1.0,
					Overrides:   map[string]string{"component": "overridden"},
				},
			},
			JobID: "test-job-4",
		}
		ctx := WithInjectionContext(context.Background(), cfg)

		inj, _ := NewInjector()
		err := inj.CheckPoint(ctx, "test.perjob")
		if err == nil {
			t.Errorf("CheckPoint() expected error, got nil")
			return
		}

		injectedErr, ok := err.(*InjectedError)
		if !ok {
			t.Errorf("CheckPoint() returned non-InjectedError: %T", err)
			return
		}

		// Verify the override was applied
		expectedMsg := "timeout error in overridden"
		if injectedErr.Message != expectedMsg {
			t.Errorf("InjectedError.Message = %q, want %q", injectedErr.Message, expectedMsg)
		}
	})
}

// TestCheckPoint_GlobalConfig tests injection with global config when no per-job config.
func TestCheckPoint_GlobalConfig(t *testing.T) {
	// Register a point
	point := &InjectionPoint{
		ID:          "test.global",
		Component:   "test",
		Action:      "global",
		Description: "Test point for global config",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeUnavailable: {
				Type:            ErrorTypeUnavailable,
				Code:            CodeUnavailable,
				MessageTemplate: "service unavailable",
			},
		},
	}
	DefaultRegistry[point.ID] = point

	// Set global config
	globalCfg := &GlobalInjectionConfig{
		Enabled: true,
		Rules: []InjectionRule{
			{
				PointRef:    "test.global",
				ErrorType:   ErrorTypeUnavailable,
				Probability: 1.0,
			},
		},
	}

	// Create context without per-job config
	ctx := context.Background()

	// CheckPoint should use global config
	inj, _ := NewInjector()
	if err := inj.SetGlobalConfig(globalCfg); err != nil {
		t.Fatalf("SetGlobalConfig() error: %v", err)
	}
	err := inj.CheckPoint(ctx, "test.global")
	if err == nil {
		t.Errorf("CheckPoint() expected error from global config, got nil")
		return
	}

	injectedErr, ok := err.(*InjectedError)
	if !ok {
		t.Errorf("CheckPoint() returned non-InjectedError: %T", err)
		return
	}

	if injectedErr.Type != ErrorTypeUnavailable {
		t.Errorf("InjectedError.Type = %v, want %v", injectedErr.Type, ErrorTypeUnavailable)
	}
}

// TestCheckPoint_Priority tests that per-job config takes precedence over global config.
func TestCheckPoint_Priority(t *testing.T) {
	// Register a point
	point := &InjectionPoint{
		ID:          "test.priority",
		Component:   "test",
		Action:      "priority",
		Description: "Test point for priority",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeTimeout: {
				Type:            ErrorTypeTimeout,
				Code:            CodeDeadlineExceeded,
				MessageTemplate: "timeout error",
			},
			ErrorTypeInternal: {
				Type:            ErrorTypeInternal,
				Code:            CodeInternal,
				MessageTemplate: "internal error",
			},
		},
	}
	DefaultRegistry[point.ID] = point

	// Set global config with internal error
	globalCfg := &GlobalInjectionConfig{
		Enabled: true,
		Rules: []InjectionRule{
			{
				PointRef:    "test.priority",
				ErrorType:   ErrorTypeInternal,
				Probability: 1.0,
			},
		},
	}

	// Create context with per-job config for timeout error (different from global)
	perJobCfg := &InjectionConfig{
		Enabled: true,
		Rules: []InjectionRule{
			{
				PointRef:    "test.priority",
				ErrorType:   ErrorTypeTimeout, // Different from global
				Probability: 1.0,
			},
		},
		JobID: "test-job-priority",
	}
	ctx := WithInjectionContext(context.Background(), perJobCfg)

	// CheckPoint should use per-job config (timeout), not global (internal)
	inj, _ := NewInjector()
	if err := inj.SetGlobalConfig(globalCfg); err != nil {
		t.Fatalf("SetGlobalConfig() error: %v", err)
	}
	err := inj.CheckPoint(ctx, "test.priority")
	if err == nil {
		t.Errorf("CheckPoint() expected error, got nil")
		return
	}

	injectedErr, ok := err.(*InjectedError)
	if !ok {
		t.Errorf("CheckPoint() returned non-InjectedError: %T", err)
		return
	}

	// Should be timeout (from per-job), not internal (from global)
	if injectedErr.Type != ErrorTypeTimeout {
		t.Errorf("InjectedError.Type = %v, want %v (per-job config should take priority)", injectedErr.Type, ErrorTypeTimeout)
	}
}

// TestSetGetGlobalConfig tests the SetGlobalConfig and GetGlobalConfig methods.
func TestSetGetGlobalConfig(t *testing.T) {
	injector, err := NewInjector()
	if err != nil {
		t.Fatalf("NewInjector() error: %v", err)
	}

	t.Run("set and get valid config", func(t *testing.T) {
		cfg := &GlobalInjectionConfig{
			Enabled: true,
			Rules: []InjectionRule{
				{
					PointRef:    "test.point",
					ErrorType:   ErrorTypeTimeout,
					Probability: 0.5,
				},
			},
			ExcludedPoints: []string{"excluded.point"},
		}

		err := injector.SetGlobalConfig(cfg)
		if err != nil {
			t.Fatalf("SetGlobalConfig() error: %v", err)
		}

		got := injector.GetGlobalConfig()
		if got == nil {
			t.Fatal("GetGlobalConfig() returned nil")
		}

		if got.Enabled != cfg.Enabled {
			t.Errorf("GetGlobalConfig().Enabled = %v, want %v", got.Enabled, cfg.Enabled)
		}

		if len(got.Rules) != len(cfg.Rules) {
			t.Errorf("GetGlobalConfig().Rules length = %d, want %d", len(got.Rules), len(cfg.Rules))
		}

		if len(got.ExcludedPoints) != len(cfg.ExcludedPoints) {
			t.Errorf("GetGlobalConfig().ExcludedPoints length = %d, want %d", len(got.ExcludedPoints), len(cfg.ExcludedPoints))
		}
	})

	t.Run("set nil config returns error", func(t *testing.T) {
		err := injector.SetGlobalConfig(nil)
		if err == nil {
			t.Error("SetGlobalConfig(nil) expected error, got nil")
		}
		if !containsString(err.Error(), "cannot be nil") {
			t.Errorf("SetGlobalConfig(nil) error = %v, want error containing 'cannot be nil'", err)
		}
	})

	t.Run("initial config is valid", func(t *testing.T) {
		inj, _ := NewInjector()
		cfg := inj.GetGlobalConfig()
		if cfg == nil {
			t.Fatal("GetGlobalConfig() returned nil for new injector")
		}
		if cfg.Enabled {
			t.Error("Initial global config should have Enabled = false")
		}
		if cfg.Rules == nil {
			t.Error("Initial global config Rules should not be nil")
		}
		if cfg.ExcludedPoints == nil {
			t.Error("Initial global config ExcludedPoints should not be nil")
		}
	})
}

// TestRenderTemplate tests the RenderTemplate method.
func TestRenderTemplate(t *testing.T) {
	injector, err := NewInjector()
	if err != nil {
		t.Fatalf("NewInjector() error: %v", err)
	}

	// Register a point with templates
	point := &InjectionPoint{
		ID:          "test.render",
		Component:   "test",
		Action:      "render",
		Description: "Test point for render",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeTimeout: {
				Type:            ErrorTypeTimeout,
				Code:            CodeDeadlineExceeded,
				MessageTemplate: "timeout after {{.duration}}ms in {{.operation}}",
				Placeholders: map[string]string{
					"duration":   "5000",
					"operation":  "default_op",
					"extra_info": "default_extra",
				},
			},
		},
	}
	DefaultRegistry[point.ID] = point

	tests := []struct {
		name        string
		pointID     string
		errorType   ErrorType
		overrides   map[string]string
		wantErr     bool
		errContains string
		checkResult func(t *testing.T, err *InjectedError)
	}{
		{
			name:      "render existing template",
			pointID:   "test.render",
			errorType: ErrorTypeTimeout,
			overrides: nil,
			wantErr:   false,
			checkResult: func(t *testing.T, err *InjectedError) {
				if err.Type != ErrorTypeTimeout {
					t.Errorf("InjectedError.Type = %v, want %v", err.Type, ErrorTypeTimeout)
				}
				if err.Code != CodeDeadlineExceeded {
					t.Errorf("InjectedError.Code = %v, want %v", err.Code, CodeDeadlineExceeded)
				}
				// Should use default placeholders
				expectedMsg := "timeout after 5000ms in default_op"
				if err.Message != expectedMsg {
					t.Errorf("InjectedError.Message = %q, want %q", err.Message, expectedMsg)
				}
			},
		},
		{
			name:      "render with overrides",
			pointID:   "test.render",
			errorType: ErrorTypeTimeout,
			overrides: map[string]string{
				"duration":  "10000",
				"operation": "custom_op",
			},
			wantErr: false,
			checkResult: func(t *testing.T, err *InjectedError) {
				expectedMsg := "timeout after 10000ms in custom_op"
				if err.Message != expectedMsg {
					t.Errorf("InjectedError.Message = %q, want %q", err.Message, expectedMsg)
				}
			},
		},
		{
			name:        "render non-existent point",
			pointID:     "nonexistent.point",
			errorType:   ErrorTypeTimeout,
			overrides:   nil,
			wantErr:     true,
			errContains: "not found",
		},
		{
			name:      "render non-existent template type (creates default)",
			pointID:   "test.render",
			errorType: ErrorTypeInternal, // Not defined in templates
			overrides: nil,
			wantErr:   false,
			checkResult: func(t *testing.T, err *InjectedError) {
				if err.Type != ErrorTypeInternal {
					t.Errorf("InjectedError.Type = %v, want %v", err.Type, ErrorTypeInternal)
				}
				// Should use default code
				if err.Code != CodeInternal {
					t.Errorf("InjectedError.Code = %v, want %v", err.Code, CodeInternal)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := injector.RenderTemplate(tt.pointID, tt.errorType, tt.overrides)

			if tt.wantErr {
				if err == nil {
					t.Errorf("RenderTemplate() expected error, got nil")
					return
				}
				if tt.errContains != "" && !containsString(err.Error(), tt.errContains) {
					t.Errorf("RenderTemplate() error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("RenderTemplate() unexpected error: %v", err)
				return
			}

			if result == nil {
				t.Errorf("RenderTemplate() returned nil result")
				return
			}

			if tt.checkResult != nil {
				tt.checkResult(t, result)
			}
		})
	}
}

// TestCheckPoint_ExcludedPoints tests that excluded points don't trigger errors.
func TestCheckPoint_ExcludedPoints(t *testing.T) {
	injector, err := NewInjector()
	if err != nil {
		t.Fatalf("NewInjector() error: %v", err)
	}

	// Register a point
	point := &InjectionPoint{
		ID:          "test.excluded",
		Component:   "test",
		Action:      "excluded",
		Description: "Test point for exclusion",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeTimeout: {
				Type:            ErrorTypeTimeout,
				Code:            CodeDeadlineExceeded,
				MessageTemplate: "timeout error",
			},
		},
	}
	DefaultRegistry[point.ID] = point

	// Set global config with the point excluded
	globalCfg := &GlobalInjectionConfig{
		Enabled: true,
		Rules: []InjectionRule{
			{
				PointRef:    "test.excluded",
				ErrorType:   ErrorTypeTimeout,
				Probability: 1.0,
			},
		},
		ExcludedPoints: []string{"test.excluded"},
	}
	if err := injector.SetGlobalConfig(globalCfg); err != nil {
		t.Fatalf("SetGlobalConfig() error: %v", err)
	}

	ctx := context.Background()

	// CheckPoint should return nil because point is excluded
	err = injector.CheckPoint(ctx, "test.excluded")
	if err != nil {
		t.Errorf("CheckPoint() on excluded point returned error: %v, expected nil", err)
	}
}

// TestCheckPoint_UnknownPoint tests that unknown points return nil.
func TestCheckPoint_UnknownPoint(t *testing.T) {
	injector, err := NewInjector()
	if err != nil {
		t.Fatalf("NewInjector() error: %v", err)
	}

	ctx := context.Background()

	// Check unknown point should return nil
	err = injector.CheckPoint(ctx, "unknown.point")
	if err != nil {
		t.Errorf("CheckPoint() on unknown point returned error: %v, expected nil", err)
	}
}

// TestCheckPoint_WildcardPattern tests wildcard pattern matching in rules.
func TestCheckPoint_WildcardPattern(t *testing.T) {
	// Define point IDs for testing
	pointIDs := []string{"wildcard.test1", "wildcard.test2", "wildcard.test3"}

	// Helper to create a point
	createPoint := func(id string) *InjectionPoint {
		return &InjectionPoint{
			ID:          id,
			Component:   "wildcard",
			Action:      id,
			Description: "Test point for wildcard",
			Templates: map[ErrorType]*InjectionTemplate{
				ErrorTypeTimeout: {
					Type:            ErrorTypeTimeout,
					Code:            CodeDeadlineExceeded,
					MessageTemplate: "timeout error",
				},
			},
		}
	}

	t.Run("global wildcard pattern", func(t *testing.T) {
		globalCfg := &GlobalInjectionConfig{
			Enabled: true,
			Rules: []InjectionRule{
				{
					PointRef:    "*", // Match all points
					ErrorType:   ErrorTypeTimeout,
					Probability: 1.0,
				},
			},
		}

		inj, _ := NewInjector()
		// Register points on new injector
		for _, id := range pointIDs {
			point := createPoint(id)
			DefaultRegistry[point.ID] = point
		}
		if err := inj.SetGlobalConfig(globalCfg); err != nil {
			t.Fatalf("SetGlobalConfig() error: %v", err)
		}

		err := inj.CheckPoint(context.Background(), "wildcard.test1")
		if err == nil {
			t.Error("CheckPoint() with wildcard pattern expected error, got nil")
		}
	})

	t.Run("component wildcard pattern", func(t *testing.T) {
		globalCfg := &GlobalInjectionConfig{
			Enabled: true,
			Rules: []InjectionRule{
				{
					PointRef:    "wildcard.*", // Match all wildcard.* points
					ErrorType:   ErrorTypeTimeout,
					Probability: 1.0,
				},
			},
		}

		inj, _ := NewInjector()
		// Register points on new injector
		for _, id := range pointIDs {
			point := createPoint(id)
			DefaultRegistry[point.ID] = point
		}
		if err := inj.SetGlobalConfig(globalCfg); err != nil {
			t.Fatalf("SetGlobalConfig() error: %v", err)
		}

		for _, id := range pointIDs {
			err := inj.CheckPoint(context.Background(), id)
			if err == nil {
				t.Errorf("CheckPoint(%q) with wildcard.* pattern expected error, got nil", id)
			}
		}
	})
}

// TestCheckPoint_CrashInjection tests that crash type triggers panic.
func TestCheckPoint_CrashInjection(t *testing.T) {
	injector, err := NewInjector()
	if err != nil {
		t.Fatalf("NewInjector() error: %v", err)
	}

	// Register a crash injection point
	point := &InjectionPoint{
		ID:          "test.crash",
		Component:   "test",
		Action:      "crash",
		Description: "Test crash point",
		Templates: map[ErrorType]*InjectionTemplate{
			ErrorTypeCrash: {
				Type:            ErrorTypeCrash,
				Code:            CodeCrash,
				MessageTemplate: "simulating crash at {{.point}}",
				Placeholders:    map[string]string{"point": "test.crash"},
			},
		},
	}
	DefaultRegistry[point.ID] = point

	cfg := &InjectionConfig{
		Enabled: true,
		Rules: []InjectionRule{
			{
				PointRef:    "test.crash",
				ErrorType:   ErrorTypeCrash,
				Probability: 1.0,
			},
		},
		JobID: "test-crash-job",
	}
	ctx := WithInjectionContext(context.Background(), cfg)

	// Test that crash injection causes panic
	defer func() {
		if r := recover(); r == nil {
			t.Error("CheckPoint() with crash type expected panic, but did not panic")
		}
	}()

	// This will panic, error return is intentionally ignored
	_ = injector.CheckPoint(ctx, "test.crash")
}

// TestListPoints tests the ListPoints method.
func TestListPoints(t *testing.T) {
	injector, err := NewInjector()
	if err != nil {
		t.Fatalf("NewInjector() error: %v", err)
	}

	clear(DefaultRegistry)

	// Initially should be empty
	points := injector.ListPoints()
	if len(points) != 0 {
		t.Errorf("ListPoints() on empty injector returned %d points, want 0", len(points))
	}

	// Register multiple points
	expectedIDs := []string{"test.list1", "test.list2", "test.list3"}
	for _, id := range expectedIDs {
		point := &InjectionPoint{
			ID:          id,
			Component:   "test",
			Action:      id,
			Description: "Test point for list",
		}

		DefaultRegistry[point.ID] = point
	}

	// List should return all points
	points = injector.ListPoints()
	if len(points) != len(expectedIDs) {
		t.Errorf("ListPoints() returned %d points, want %d", len(points), len(expectedIDs))
	}

	// Verify all expected IDs are present
	foundIDs := make(map[string]bool)
	for _, p := range points {
		foundIDs[p.ID] = true
	}
	for _, id := range expectedIDs {
		if !foundIDs[id] {
			t.Errorf("ListPoints() missing expected point %q", id)
		}
	}
}

// Helper function to check if a string contains a substring
func containsString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
