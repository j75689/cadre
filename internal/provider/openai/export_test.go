package openai

import "github.com/openai/openai-go/v3/option"

// The fake API scripts every reply, so an SDK retry would hit it as an
// unscripted request.
func init() { extraOptions = []option.RequestOption{option.WithMaxRetries(0)} }
