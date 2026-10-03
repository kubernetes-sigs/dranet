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

package driver

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNRIPluginDisconnectsTotal(t *testing.T) {
	before := testutil.ToFloat64(nriPluginDisconnectsTotal)

	nriPluginDisconnectsTotal.Inc()
	nriPluginDisconnectsTotal.Inc()

	if got := testutil.ToFloat64(nriPluginDisconnectsTotal) - before; got != 2 {
		t.Errorf("expected nriPluginDisconnectsTotal to increase by 2, got %v", got)
	}
}

func TestReconnectAttemptGauges(t *testing.T) {
	nriPluginReconnectAttempt.Set(3)
	netdbReconnectAttempt.Set(1)

	if got := testutil.ToFloat64(nriPluginReconnectAttempt); got != 3 {
		t.Errorf("expected nri_plugin reconnect attempt to be 3, got %v", got)
	}
	if got := testutil.ToFloat64(netdbReconnectAttempt); got != 1 {
		t.Errorf("expected netdb reconnect attempt to be 1, got %v", got)
	}

	expectedNRI := `
		# HELP dranet_driver_nri_plugin_reconnect_attempt Current attempt number (0-indexed) in the restart loop for the NRI plugin connection. Resets to 0 on process restart; approaches maxAttempts before the process exits.
		# TYPE dranet_driver_nri_plugin_reconnect_attempt gauge
		dranet_driver_nri_plugin_reconnect_attempt 3
		`
	if err := testutil.CollectAndCompare(nriPluginReconnectAttempt, strings.NewReader(expectedNRI), "dranet_driver_nri_plugin_reconnect_attempt"); err != nil {
		t.Fatalf("CollectAndCompare failed for nriPluginReconnectAttempt: %v", err)
	}

	expectedNetdb := `
		# HELP dranet_driver_netdb_reconnect_attempt Current attempt number (0-indexed) in the restart loop for the host network device database. Resets to 0 on process restart; approaches maxAttempts before the process exits.
		# TYPE dranet_driver_netdb_reconnect_attempt gauge
		dranet_driver_netdb_reconnect_attempt 1
		`
	if err := testutil.CollectAndCompare(netdbReconnectAttempt, strings.NewReader(expectedNetdb), "dranet_driver_netdb_reconnect_attempt"); err != nil {
		t.Fatalf("CollectAndCompare failed for netdbReconnectAttempt: %v", err)
	}
}
