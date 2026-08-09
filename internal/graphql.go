package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/bytedance/sonic"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/printer"
	"github.com/graphql-go/graphql/language/source"
	"github.com/graphql-go/graphql/language/visitor"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/exp/rand"
)

// batchedgqlclient accumulates queries and executes them in batch in order to
// make better use of RPS limits.
type batchedgqlclient struct {
	mu sync.Mutex

	batchSize       int            // batchSize is the max number of queries per batch.
	searchBatchSize int            // searchBatchSize caps search queries per batch; <= 0 disables search isolation.
	queue           []batchedQuery // queue contains spillover in cases where we've accumulated more queries than our batch size allows.
	every           time.Duration  // every controls how often requests are flushed.
	metrics         *gqlMetrics    // metrics tracks batches and queries sent.

	wrapped graphql.Client
}

// NewBatchedGraphQLClient creates a batching GraphQL client. Queries are
// accumulated and executed regularly according to the given rate.
func NewBatchedGraphQLClient(url string, client *http.Client, every time.Duration, batchSize int, searchBatchSize int, reg *prometheus.Registry) (graphql.Client, error) {
	wrapped := graphql.NewClient(url, client)

	c := &batchedgqlclient{
		batchSize:       batchSize,
		searchBatchSize: searchBatchSize,
		wrapped:         wrapped,
		queue:           []batchedQuery{},
		metrics:         newGQLMetrics(reg),
		every:           every,
	}

	go func() {
		ctx := context.WithValue(context.Background(), middleware.RequestIDKey, fmt.Sprintf("batch-flush-%d", time.Now().Unix()))
		for {
			time.Sleep(c.every)
			c.flush(ctx)
		}
	}()

	// Log gql stats every minute.
	go func() {
		ctx := context.Background()
		for {
			time.Sleep(1 * time.Minute)
			batchesWaiting := c.metrics.batchesWaitingGet()
			batchesSent := c.metrics.batchesSentGet()
			queriesSent := c.metrics.queriesSentGet()

			Log(ctx).Debug("query stats",
				"batchesWaiting", batchesWaiting,
				"batchesSent", batchesSent,
				"queriesSent", queriesSent,
				"averageBatchSize", (float32(queriesSent) / float32(batchesSent)),
			)
		}
	}()

	return c, nil
}

// flush drains one batch from the queue per tick to bound upstream request
// rate. High-priority batches (interactive search chains) are flushed ahead
// of background work (author hydration). Firing every queued batch concurrently
// bursts past Hardcover's rate limit and produces 429s.
func (c *batchedgqlclient) flush(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.metrics.batchesWaitingSet(len(c.queue))

	if len(c.queue) == 0 {
		return
	}

	// Flush the first priority batch; if none, flush the first batch.
	idx := 0
	for i, b := range c.queue {
		if b.isPriority {
			idx = i
			break
		}
	}

	batch := c.queue[idx]
	c.queue = append(c.queue[:idx], c.queue[idx+1:]...)
	c.fire(ctx, batch)
}

// fire executes a single batch as one GraphQL request. It is called under
// c.mu; the upstream HTTP call happens in a spawned goroutine that does not
// touch c.mu-protected state.
func (c *batchedgqlclient) fire(ctx context.Context, batch batchedQuery) {
	c.metrics.batchesSentInc()
	c.metrics.queriesSentAdd(int64(len(batch.subscribers)))

	query, vars, err := batch.qb.build()
	if err != nil {
		Log(ctx).Error("unable to build query", "err", err)
		return
	}

	data := map[string]any{}
	req := &graphql.Request{
		Query:     query,
		Variables: vars,
		OpName:    batch.qb.op.Name.Value,
	}
	resp := &graphql.Response{
		Data: &data,
	}

	// Issue the request in a separate goroutine so we can continue to
	// accumulate queries without needing to wait for the network call.
	go func(batch batchedQuery) {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()

		err := c.wrapped.MakeRequest(ctx, req, resp)

		// Extract any field-level errors, and return them to their
		// subscribers. We can ignore the top-level err in this case, because
		// it's just the wrapped version of our response errors.
		if resp != nil && len(resp.Errors) > 0 {
			for _, e := range resp.Errors {
				sub, ok := batch.subscribers[e.Path.String()]
				if !ok {
					continue
				}
				sub.respC <- gqlStatusErr(e)
				// Remove our subscriber because we already responded.
				delete(batch.subscribers, e.Path.String())
			}
		} else if err != nil {
			// For everything else return the status code to all our subscribers.
			Log(ctx).Warn("batched query error", "count", len(batch.subscribers), "err", err, "resp.Errors", resp.Errors)
			for _, sub := range batch.subscribers {
				sub.respC <- gqlStatusErr(err)
			}
			return
		}

		for id, sub := range batch.subscribers {
			// TODO: missing response.
			byt, err := json.Marshal(map[string]any{
				sub.field: data[id],
			})
			if err != nil {
				sub.respC <- err
				continue
			}

			sub.respC <- sonic.ConfigStd.Unmarshal(byt, &sub.resp.Data)
		}
	}(batch)
}

// MakeRequest implements graphql.Client.
func (c *batchedgqlclient) MakeRequest(
	ctx context.Context,
	req *graphql.Request,
	resp *graphql.Response,
) error {
	err := <-c.enqueue(ctx, req, resp).respC
	return err
}

// enqueue adds a query to the batch and returns a subscription whose result
// channel resolves when the batch is executed.
func (c *batchedgqlclient) enqueue(
	ctx context.Context,
	req *graphql.Request,
	resp *graphql.Response,
) *subscription {
	// Determine whether this is a search query so it can be routed into a
	// dedicated, smaller batch. When searchBatchSize is disabled every query
	// is treated as non-search and behavior is exactly as before.
	isSearch := false
	if c.searchBatchSize > 0 {
		field, err := topLevelField(req.Query)
		isSearch = err == nil && field == "search"
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Take the youngest batch of the same kind with spare capacity, otherwise
	// start a new batch. Search queries never share a batch with other
	// queries, and search batches are capped at searchBatchSize.
	//
	// Priority is propagated from the request context: interactive requests
	// (search chains) carry WithRequestPriority so they queue ahead of
	// background work (author hydration).
	isPriority := RequestPriorityFromContext(ctx)

	idx := -1
	for i := len(c.queue) - 1; i >= 0; i-- {
		b := c.queue[i]
		if b.isSearch != isSearch {
			continue
		}
		if b.isPriority != isPriority {
			continue
		}
		limit := c.batchSize
		if b.isSearch {
			limit = c.searchBatchSize
		}
		if len(b.subscribers) < limit {
			idx = i
			break
		}
	}
	if idx == -1 {
		c.queue = append(c.queue, batchedQuery{
			qb:          newQueryBuilder(),
			subscribers: map[string]*subscription{},
			isSearch:    isSearch,
			isPriority:  isPriority,
		})
		idx = len(c.queue) - 1
	}
	batch := c.queue[idx]

	respC := make(chan error, 1)

	sub := &subscription{
		ctx:   ctx,
		resp:  resp,
		respC: respC,
	}

	var vars map[string]any
	out, _ := json.Marshal(req.Variables)
	_ = sonic.ConfigStd.Unmarshal(out, &vars)

	id, field, err := batch.qb.add(req.Query, vars)
	if err != nil {
		respC <- err
	}

	batch.subscribers[id] = &subscription{
		ctx:   ctx,
		resp:  resp,
		respC: respC,
		field: field,
	}

	return sub
}

// requestPriorityKey is the context key for request priority.
type requestPriorityKey struct{}

// RequestPriorityFromContext returns the request priority from a context.
// Set to true for interactive requests (search chains) so they queue ahead
// of background work (author hydration).
func RequestPriorityFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(requestPriorityKey{}).(bool)
	return v
}

// WithRequestPriority returns a context marked as high priority.
func WithRequestPriority(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestPriorityKey{}, true)
}

// subscription holds information about a caller who is waiting for a query to
// be resolved as part of a batch.
type subscription struct {
	ctx   context.Context
	resp  *graphql.Response
	respC chan error
	field string
}

// gqlStatusErr translates errors into meaningful status codes. The client
// normally returns error responses with a 200 OK status code and a populated
// "Errors" field containing stringed errors. We want to instead surface e.g.
// 404 errors directly.
//
// The error is returned unchanged if it doesn't include a status code.
func gqlStatusErr(err error) error {
	errStr := err.Error()
	idx := strings.Index(errStr, "Request failed with status code")
	if idx == -1 {
		return err
	}
	code, _ := pathToID(errStr[idx:])
	return errors.Join(err, statusErr(code))
}

// queryBuilder accumulates queries into one query with multiple fields so they
// can all be executed as part of one request.
type queryBuilder struct {
	op        *ast.OperationDefinition
	fragments map[string]struct{}
	vars      map[string]any
}

type batchedQuery struct {
	qb          *queryBuilder
	subscribers map[string]*subscription
	isSearch    bool // isSearch marks batches reserved exclusively for search queries.
	isPriority  bool // isPriority marks batches from interactive requests that should queue ahead.
}

// _fragments holds string representations of fragment nodes since they are static.
var _fragments = map[string]string{}

// newQueryBuilder initializes a new QueryBuilder with an empty Document.
func newQueryBuilder() *queryBuilder {
	return &queryBuilder{
		vars:      make(map[string]any),
		fragments: map[string]struct{}{},
	}
}

var runes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

// randRunes returns a short random string of length n.
func randRunes(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = runes[rand.Intn(len(runes))]
	}
	return string(b)
}

// topLevelField returns the name of the query's first top-level selection
// field (e.g. "search" or "books_by_pk").
func topLevelField(query string) (string, error) {
	parsedDoc, err := parser.Parse(parser.ParseParams{
		Source: source.NewSource(&source.Source{Body: []byte(query)}),
	})
	if err != nil {
		return "", err
	}
	for _, def := range parsedDoc.Definitions {
		opDef, ok := def.(*ast.OperationDefinition)
		if !ok {
			continue
		}
		for _, sel := range opDef.SelectionSet.Selections {
			if f, ok := sel.(*ast.Field); ok {
				return f.Name.Value, nil
			}
		}
	}
	return "", nil
}

// add extends the current query with a new field. The field's alias and name
// are returned so they can be recovered later.
func (qb *queryBuilder) add(query string, vars map[string]any) (id string, field string, err error) {
	src := source.NewSource(&source.Source{
		Body: []byte(query),
	})

	parsedDoc, err := parser.Parse(parser.ParseParams{Source: src})
	if err != nil {
		return "", "", fmt.Errorf("failed to parse query: %w", err)
	}

	id = randRunes(8)

	varRename := make(map[string]string)

	// TODO: Only handle one def
	for _, def := range parsedDoc.Definitions {
		// Include fragments, if there are any, and cache their strings because
		// they don't change.
		if fragDef, ok := def.(*ast.FragmentDefinition); ok {
			name := fragDef.Name.Value
			if _, seen := qb.fragments[name]; !seen {
				if _, cached := _fragments[name]; !cached {
					_fragments[name] = printer.Print(fragDef).(string)
				}
				qb.fragments[name] = struct{}{}
			}
		}

		opDef, ok := def.(*ast.OperationDefinition)
		if !ok {
			continue
		}

		if qb.op == nil {
			qb.op = opDef
		}

		// Visit the AST to rename vars and alias fields
		opts := visitor.VisitInParallel(&visitor.VisitorOptions{
			Enter: func(p visitor.VisitFuncParams) (string, any) {
				switch node := p.Node.(type) {
				case *ast.VariableDefinition:
					oldName := node.Variable.Name.Value
					newName := id + "_" + oldName
					varRename[oldName] = newName
					node.Variable.Name.Value = newName
					qb.vars[newName] = vars[oldName]
				case *ast.Variable:
					if newName, ok := varRename[node.Name.Value]; ok {
						node.Name.Value = newName
					}
				case *ast.Field:
					if len(p.Ancestors) == 3 {
						field = node.Name.Value
						node.Alias = &ast.Name{Value: id, Kind: "Name"}
					}
				}
				return visitor.ActionNoChange, nil
			},
		})
		visitor.Visit(opDef, opts, nil)

		if qb.op == opDef {
			continue
		}

		qb.op.SelectionSet.Selections = append(qb.op.SelectionSet.Selections, opDef.SelectionSet.Selections...)
		qb.op.VariableDefinitions = append(qb.op.VariableDefinitions, opDef.VariableDefinitions...)
	}

	return id, field, nil
}

// Build returns the merged query string and variables map.
func (qb *queryBuilder) build() (string, map[string]any, error) {
	builder := strings.Builder{}

	builder.WriteString(printer.Print(qb.op).(string))

	for _, fragName := range slices.Sorted(maps.Keys(qb.fragments)) {
		builder.WriteString("\n")
		builder.WriteString(_fragments[fragName])
	}

	return builder.String(), qb.vars, nil
}
