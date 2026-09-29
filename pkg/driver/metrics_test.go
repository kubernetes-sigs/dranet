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

func TestReconnectAttemptGauge(t *testing.T) {
	reconnectAttempt.Reset()

	reconnectAttempt.WithLabelValues("nri_plugin").Set(3)
	reconnectAttempt.WithLabelValues("netdb").Set(1)

	if got := testutil.ToFloat64(reconnectAttempt.WithLabelValues("nri_plugin")); got != 3 {
		t.Errorf("expected nri_plugin reconnect attempt to be 3, got %v", got)
	}
	if got := testutil.ToFloat64(reconnectAttempt.WithLabelValues("netdb")); got != 1 {
		t.Errorf("expected netdb reconnect attempt to be 1, got %v", got)
	}

	expected := `
		# HELP dranet_driver_reconnect_attempt Current attempt number (0-indexed) in the restart loop for a dranet subsystem. Resets to 0 on process restart; approaches maxAttempts before the process exits.
		# TYPE dranet_driver_reconnect_attempt gauge
		dranet_driver_reconnect_attempt{component="netdb"} 1
		dranet_driver_reconnect_attempt{component="nri_plugin"} 3
		`
	if err := testutil.CollectAndCompare(reconnectAttempt, strings.NewReader(expected), "dranet_driver_reconnect_attempt"); err != nil {
		t.Fatalf("CollectAndCompare failed: %v", err)
	}
}
