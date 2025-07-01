/*
Copyright 2024 The Aibrix Team.

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
package types

type OutputPredictor interface {
	// AddTrace collects history input and output tokens data.
	AddTrace(inputTokens, outputTokens int, cnt int32)

	// Predict outputs the number of output tokens based on the number of input tokens.
	Predict(promptLen int) (outputLen int)
}

// OutputPredictorProvider provides a stateful way to get an output predictor, allowing a struct to provide the output predictor by model.
type OutputPredictorProvider interface {
	// GetOutputPredictor returns the output predictor
	GetOutputPredictor(modelName string) (OutputPredictor, error)
}
