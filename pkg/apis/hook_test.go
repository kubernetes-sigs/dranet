/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apis

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRuntimeHookValidate(t *testing.T) {
	testCases := []struct {
		name        string
		hook        RuntimeHook
		wantTimeout int
		wantErr     string
	}{
		{name: "defaults the timeout", hook: RuntimeHook{Path: "/opt/acme/hook"}, wantTimeout: RuntimeHookDefaultTimeoutSeconds},
		{name: "keeps a valid timeout", hook: RuntimeHook{Path: "/opt/acme/hook", TimeoutSeconds: 20}, wantTimeout: 20},
		{name: "accepts data", hook: RuntimeHook{Path: "/opt/acme/hook", Data: json.RawMessage(`{"a":1}`)}, wantTimeout: RuntimeHookDefaultTimeoutSeconds},
		{name: "empty path", hook: RuntimeHook{}, wantErr: "path is empty"},
		{name: "relative path", hook: RuntimeHook{Path: "acme/hook"}, wantErr: "not absolute"},
		{name: "negative timeout", hook: RuntimeHook{Path: "/opt/acme/hook", TimeoutSeconds: -1}, wantErr: "negative"},
		{name: "timeout over the maximum", hook: RuntimeHook{Path: "/opt/acme/hook", TimeoutSeconds: RuntimeHookMaxTimeoutSeconds + 1}, wantErr: "over the maximum"},
		{name: "invalid data", hook: RuntimeHook{Path: "/opt/acme/hook", Data: json.RawMessage(`{`)}, wantErr: "not valid JSON"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.hook.Validate()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.hook.TimeoutSeconds != tc.wantTimeout {
				t.Fatalf("timeout = %d, want %d", tc.hook.TimeoutSeconds, tc.wantTimeout)
			}
		})
	}
}
