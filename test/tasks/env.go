// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"errors"
	"fmt"
	"math"
	"strconv"

	"latere.ai/x/topos/models"
)

// The environment of a run against a real model: the connection that
// topos run reads, the model the suite runs, and the run's shape.
const (
	EnvModelsURL = "TOPOS_MODELS_URL"
	EnvModelsKey = "TOPOS_MODELS_KEY"
	EnvModel     = "TOPOS_TASKS_MODEL"
	EnvFilter    = "TOPOS_TASKS_FILTER"
	EnvRuns      = "TOPOS_TASKS_RUNS"
	EnvBudget    = "TOPOS_TASKS_BUDGET"
	EnvCommit    = "TOPOS_TASKS_COMMIT"
	EnvOut       = "TOPOS_TASKS_OUT"
)

// ErrNoModel is an environment that names no model connection.
var ErrNoModel = fmt.Errorf("the task suite runs against a real model: set %s, %s and %s", EnvModelsURL, EnvModelsKey, EnvModel)

// FromEnv reads the options of a run against a real model: the
// connection, the model, the filter, the runs per task, the suite's
// budget in USD and the commit. It returns ErrNoModel when the
// connection, the credential or the model is missing.
func FromEnv(getenv func(string) string) (Options, error) {
	o := Options{
		Connection: models.Connection{BaseURL: getenv(EnvModelsURL), Credential: getenv(EnvModelsKey), Model: getenv(EnvModel)},
		Filter:     getenv(EnvFilter),
		Commit:     getenv(EnvCommit),
	}
	if o.Connection.BaseURL == "" || o.Connection.Credential == "" || o.Connection.Model == "" {
		return Options{}, ErrNoModel
	}
	if o.Connection.Scripted() {
		return Options{}, errors.New("tasks: the scripted model proves the checkers, not agent behavior; name a real model")
	}
	if err := o.Connection.Validate(); err != nil {
		return Options{}, err
	}
	if v := getenv(EnvRuns); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Options{}, fmt.Errorf("tasks: %s=%q is not a count of at least 1", EnvRuns, v)
		}
		o.Runs = n
	}
	if v := getenv(EnvBudget); v != "" {
		usd, err := strconv.ParseFloat(v, 64)
		if err != nil || usd <= 0 || math.IsInf(usd, 0) {
			return Options{}, fmt.Errorf("tasks: %s=%q is not a positive amount in USD", EnvBudget, v)
		}
		o.BudgetUSDMicro = int64(math.Round(usd * 1e6))
	}
	return o, nil
}
