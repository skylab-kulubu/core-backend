package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/erasure"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// erasureManualStepCommandName records a service erasure step an operator
// carried out by hand (docs/account-lifecycle.md, A service without its
// endpoint). The worker's call to a service that has no erase endpoint yet
// gets 404 and the request goes to manual intervention under
// erase_<service>_rejected_404; the operator erases the person's data in that
// service directly, then runs this inside the core container:
//
//	core-backend erasure-manual-step --request <request_id> --service cms --counts '{"actor_columns_replaced":2}'
//
// It checkpoints the step with those counts and operator_recorded=1 and puts
// the request back to pending, so the worker goes on with the saga. It takes
// only the request id, never the subject or an address, and prints the
// record: request id, step, time and counts.
const erasureManualStepCommandName = "erasure-manual-step"

// Exit codes beyond 0 (recorded), 1 (the database failed; nothing changed)
// and 2 (usage or configuration).
const erasureManualStepExitRefused = 3

type erasureManualStepOptions struct {
	requestID uuid.UUID
	service   erasure.Service
	counts    map[string]int64
}

func runErasureManualStep(args []string, getenv func(string) string, out, errOut io.Writer) int {
	options, code := parseErasureManualStepOptions(args, errOut)
	if code != 0 {
		return code
	}
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		fmt.Fprintf(errOut, "%s needs DATABASE_URL\n", erasureManualStepCommandName)
		return 2
	}
	ctx, stop := commandContext()
	defer stop()
	pool, err := pgxpool.New(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(errOut, "database: cannot open the connection pool")
		return 1
	}
	defer pool.Close()
	return erasureManualStepCommand(ctx, out, errOut, time.Now().UTC(), options, user.NewPostgresStore(pool).RecordOperatorErasureStep)
}

func parseErasureManualStepOptions(args []string, errOut io.Writer) (erasureManualStepOptions, int) {
	flags := flag.NewFlagSet(erasureManualStepCommandName, flag.ContinueOnError)
	flags.SetOutput(errOut)
	request := flags.String("request", "", "the deletion request's id (request_id), never the subject")
	service := flags.String("service", "", "the service whose erasure the operator carried out: skymail, cms or forms")
	counts := flags.String("counts", "", `what the operator's change reported, as the service would: {"actor_columns_replaced":2}`)
	if err := flags.Parse(args); err != nil {
		return erasureManualStepOptions{}, 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "%s takes no argument beyond its flags\n", erasureManualStepCommandName)
		return erasureManualStepOptions{}, 2
	}
	var options erasureManualStepOptions
	var err error
	if options.requestID, err = uuid.Parse(strings.TrimSpace(*request)); err != nil {
		fmt.Fprintln(errOut, "--request must be the deletion request's id (a UUID)")
		return erasureManualStepOptions{}, 2
	}
	var ok bool
	if options.service, ok = erasure.ServiceNamed(strings.TrimSpace(*service)); !ok {
		fmt.Fprintln(errOut, "--service must be skymail, cms or forms")
		return erasureManualStepOptions{}, 2
	}
	if strings.TrimSpace(*counts) == "" {
		fmt.Fprintln(errOut, `--counts is required: what the change reported, {"actor_columns_replaced":0} when it found nothing`)
		return erasureManualStepOptions{}, 2
	}
	if options.counts, err = erasure.ParseCounts(json.RawMessage(*counts)); err != nil {
		fmt.Fprintf(errOut, "--counts: %v\n", err)
		return erasureManualStepOptions{}, 2
	}
	if _, set := options.counts[user.OperatorRecordedCount]; set {
		fmt.Fprintf(errOut, "--counts: %s is added by the command\n", user.OperatorRecordedCount)
		return erasureManualStepOptions{}, 2
	}
	return options, 0
}

// erasureManualStepCommand records the step and prints the record. Neither
// out nor errOut carries the subject or an address: the command never has
// them.
func erasureManualStepCommand(ctx context.Context, out, errOut io.Writer, now time.Time, options erasureManualStepOptions,
	record func(context.Context, uuid.UUID, user.DeletionStep, time.Time, map[string]int64) error) int {
	step := options.service.Step
	err := record(ctx, options.requestID, step, now, options.counts)
	switch {
	case errors.Is(err, user.ErrNotFound):
		fmt.Fprintf(errOut, "no deletion request with request_id=%s; nothing changed\n", options.requestID)
		return erasureManualStepExitRefused
	case errors.Is(err, user.ErrOperatorStepRefused):
		fmt.Fprintf(errOut, "request_id=%s is not in manual_intervention under a %s_… code, or %s is checkpointed already; nothing changed\n",
			options.requestID, step, step)
		return erasureManualStepExitRefused
	case err != nil:
		fmt.Fprintf(errOut, "database: %v; nothing changed, run it again\n", err)
		return 1
	}
	recorded := maps.Clone(options.counts)
	recorded[user.OperatorRecordedCount] = 1
	raw, _ := json.Marshal(recorded)
	fmt.Fprintln(out, "account erasure step recorded by an operator")
	fmt.Fprintf(out, "request_id: %s\n", options.requestID)
	fmt.Fprintf(out, "step: %s\n", step)
	fmt.Fprintf(out, "at: %s\n", now.Format(time.RFC3339))
	fmt.Fprintf(out, "counts: %s\n", raw)
	fmt.Fprintln(out, "request: back to pending; the worker goes on with the saga")
	return 0
}
