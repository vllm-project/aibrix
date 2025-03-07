package tests

import (
	"context"
	"fmt"
	"testing"
	"net"
	"net/http"

	"github.com/gruntwork-io/terratest/modules/terraform"
	"github.com/stretchr/testify/assert"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

func TestAIBrixGCPDeployment(t *testing.T) {
	t.Parallel()

	clusterName := "aibrix-test-cluster"
	modelName := "deepseek-r1-distill-llama-8b"

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

	// Create OpenAI client
	client := openai.NewClient(
		option.WithBaseURL(fmt.Sprintf("http://%s", aibrixServicePublicIp)),
		option.WithMiddleware(func(r *http.Request, mn option.MiddlewareNext) (*http.Response, error) {
			r.URL.Path = "/v1" + r.URL.Path
			return mn(r)
		}),
		option.WithMaxRetries(0),
	)

	chatCompletion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Messages: openai.F([]openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("What can you tell me about San Francisco?"),
		}),
		Model: openai.F(modelName),
	})
	if err != nil {
		t.Fatalf("chat completions failed: %v", err)
	}

	assert.Equal(t, modelName, chatCompletion.Model, fmt.Sprintf("expecting chat completions model to be %v", modelName))
	assert.NotEmpty(t, chatCompletion.Choices, "chat completion has no choices returned")
	assert.NotNil(t, chatCompletion.Choices[0].Message.Content, "chat completion has no message returned")
}
