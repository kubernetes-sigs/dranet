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

package main

import (
	"bytes"
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/util/sets"
)

type healthChecker struct {
	name  string
	check func() error
}

// registerHealthz installs Kubernetes apiserver-style healthz endpoints on mux:
//   - /healthz evaluates the logical AND of all checks (supporting ?verbose and
//     repeated ?exclude=<name> query parameters).
//   - /healthz/<name> evaluates an individual check.
func registerHealthz(mux *http.ServeMux, checks ...healthChecker) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		verbose := r.URL.Query().Has("verbose")
		excluded := sets.New(r.URL.Query()["exclude"]...)

		var out bytes.Buffer
		failed := false
		for _, c := range checks {
			if excluded.Has(c.name) {
				fmt.Fprintf(&out, "[+]%s excluded: ok\n", c.name)
				continue
			}
			if err := c.check(); err != nil {
				failed = true
				fmt.Fprintf(&out, "[-]%s failed: %v\n", c.name, err)
				continue
			}
			fmt.Fprintf(&out, "[+]%s ok\n", c.name)
		}

		if failed {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(&out, "healthz check failed\n")
			_, _ = w.Write(out.Bytes())
			return
		}

		w.WriteHeader(http.StatusOK)
		if verbose {
			fmt.Fprint(&out, "healthz check passed\n")
			_, _ = w.Write(out.Bytes())
			return
		}
		_, _ = fmt.Fprint(w, "ok")
	})

	for _, c := range checks {
		mux.HandleFunc("/healthz/"+c.name, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")

			if err := c.check(); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprintf(w, "[-]%s failed: %v\n", c.name, err)
				return
			}
			w.WriteHeader(http.StatusOK)
			if r.URL.Query().Has("verbose") {
				fmt.Fprintf(w, "[+]%s ok\n", c.name)
				return
			}
			_, _ = fmt.Fprint(w, "ok")
		})
	}
}
