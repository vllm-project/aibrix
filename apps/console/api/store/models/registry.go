/*
Copyright 2025 The Aibrix Team.

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

package models

import "gorm.io/gorm"

// registeredModels holds all models that should be auto-migrated.
var registeredModels []any

// RegisterModel adds a model to the migration registry.
// Call this in init() of each model file.
func RegisterModel(model any) {
	registeredModels = append(registeredModels, model)
}

// AllModels returns all registered models for AutoMigrate.
func AllModels() []any {
	return registeredModels
}

// AutoMigrate runs migrations for all registered models.
func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(registeredModels...)
}
