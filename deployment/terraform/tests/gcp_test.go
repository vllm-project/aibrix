package tests

import (
	"testing"
	"net"

	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/stretchr/testify/assert"

	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func TestAIBrixGCPDeployment(t *testing.T) {
	t.Parallel()

	clusterName := "aibrix-test-cluster"

	terraformOptions := terraform.WithDefaultRetryableErrors(t, &terraform.Options{
		// The path to where our Terraform code is located
		TerraformDir: "../gcp",

		// Variables to pass to our Terraform code using -var-file options
		VarFiles: []string{"../gcp/terraform.tfvars"},

		// Variables to pass to our Terraform code using -var options
		Vars: map[string]interface{}{
			"cluster_name": clusterName,
		},

		// Disable colors in Terraform commands so its easier to parse stdout/stderr
		NoColor: true,
	})

	// Clean up resources with "terraform destroy". Using "defer" runs the command at the end of the test, whether the test succeeds or fails.
	// At the end of the test, run `terraform destroy` to clean up any resources that were created
	defer terraform.Destroy(t, terraformOptions)

	// Run "terraform init" and "terraform apply".
	// This will run `terraform init` and `terraform apply` and fail the test if there are any errors
	terraform.InitAndApply(t, terraformOptions)

	// Run `terraform output` to get the IP address of the service
	aibrixServicePublicIp := terraform.Output(t, terraformOptions, "aibrix_service_public_ip")

	// Verify the terraform output is a valid IP address
	assert.NotNil(t, net.ParseIP(aibrixServicePublicIp), "expecting output:aibrix_service_public_ip to be valid IP")

	// Construct url from validated ip
	url := fmt.Sprintf("http://%s/v1/completions", aibrixServicePublicIp)

	// Define the request payload
	payload := map[string]string{
		"model": "deepseek-r1-distill-llama-8b",
        "prompt": "San Francisco is a",
        "max_tokens": "128",
        "temperature": "0",
	}

	// Convert payload to JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		t.Fatal("Test failed due to error: Error marshalling request JSON: ", err)
	}

	// Create a new HTTP request for model service
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		t.Fatal("Test failed due to error: Error creating request: ", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")

	// Create an HTTP client and send the request to the model service
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("Test failed due to error: Error sending request: ", err)
	}
	defer resp.Body.Close()

	// Read the response body from the model service
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal("Test failed due to error: Error reading response body: ", err)
	}

	// Assertions based on response from the model service
	assert.Equal(t, "200 OK", resp.Status, "expecting 200 OK response")
	assert.NotNil(t, body, "expecting body to be defined")

	var data map[string]interface{}

	// Decoding response body
	err = json.Unmarshal(body, &data)
	if err != nil {
		t.Fatal("Test failed due to error: Error decoding response body: ", err)
	}

	//Assertions based on response data
	assert.Equal(t, "deepseek-r1-distill-llama-8b", data["model"], "expecting model to be deepseek-r1-distill-llama-8b")
	assert.Equal(t, "text_completion", data["object"], "expecting object to be text_completion")
	assert.NotNil(t, data["choices"], "expecting choices to be defined")

	choices := data["choices"].([]interface{})
	assert.NotNil(t, choices[0], "expecting choices[0] to be defined")

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		t.Fatal("Test failed due to error: Error decoding choices[0]")
	}

	// Asserting we get a response text back from the model
	assert.NotNil(t, choice["text"], "expecting response text to be defined")
}
