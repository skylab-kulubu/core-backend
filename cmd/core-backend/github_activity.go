package main

import (
	"strconv"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/githubactivity"
	"github.com/skylab-kulubu/core-backend/internal/handlers"
)

// githubActivityFromEnv is the admin dashboard's GitHub activity
// (docs/github-activity.md). Unset, it is nil and the route is not served
// (404). Set but wrong, the route answers 503 and the line says which setting
// is wrong: a dashboard section never stops core from starting. The private
// key is never logged.
func githubActivityFromEnv(getenv func(string) string, az authz.Authorizer, logf func(string, ...any)) handlers.GithubActivitySource {
	config, ok, err := githubactivity.ConfigFromEnv(getenv)
	switch {
	case !ok:
		logf("github activity: off (%s, %s and %s are not set); /v1/dashboard/github-activity answers 404",
			githubactivity.OrgEnv, githubactivity.AppIDEnv, githubactivity.PrivateKeyEnv)
		return nil
	case err != nil:
		logf("github activity: settings are wrong, /v1/dashboard/github-activity answers 503: %v", err)
		return githubactivity.Unavailable(az, err)
	}
	installation := "found from the organisation"
	if config.InstallationID != 0 {
		installation = strconv.FormatInt(config.InstallationID, 10)
	}
	logf("github activity: on (org %s, app %d, installation %s, window %d days; %d workers, %d pages a list, %s a read)",
		config.Org, config.AppID, installation, config.WindowDays, config.Workers, config.MaxPages, config.RefreshTimeout)
	return githubactivity.New(config, az, githubactivity.Options{})
}
