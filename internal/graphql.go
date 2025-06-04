package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/printer"
	"github.com/graphql-go/graphql/language/source"
	"github.com/graphql-go/graphql/language/visitor"
	"golang.org/x/exp/rand"
)

// batchedgqlclient accumulates queries and executes them in batch in order to
// make better use of RPS limits.
type batchedgqlclient struct {
	mu sync.Mutex

	batchSize int            // batchSize is the max number of queries per batch.
	queue     []batchedQuery // queue contains spillover in cases where we've accumulated more queries than our batch size allows.

	wrapped graphql.Client
}

// NewBatchedGraphQLClient creates a batching GraphQL client. Queries are
// accumulated and executed regularly accurding to the given rate.
func NewBatchedGraphQLClient(url string, client *http.Client, rate time.Duration, batchSize int) (graphql.Client, error) {
	wrapped := graphql.NewClient(url, client)

	c := &batchedgqlclient{
		batchSize: batchSize,
		wrapped:   wrapped,
		queue:     []batchedQuery{},
	}

	go func() {
		ctx := context.WithValue(context.Background(), middleware.RequestIDKey, fmt.Sprintf("batch-flush-%d", time.Now().Unix()))
		for {
			time.Sleep(rate)
			c.flush(ctx)
		}
	}()
	return c, nil
}

// flush pops the oldest batchedQuery off the queue and executes it.
// Individualized errors are returned to listeners if possible, so one query
// can fail without the entire batch failing. The whole batch can still fail in
// other cases, e.g. 4XX response codes.
func (c *batchedgqlclient) flush(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.queue) == 0 {
		return // Nothing to do yet.
	}

	// Take our oldest batch off the queue.
	batch := c.queue[0]
	c.queue = c.queue[1:]

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

			sub.respC <- json.Unmarshal(byt, &sub.resp.Data)
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
	c.mu.Lock()
	defer c.mu.Unlock()

	// Take the youngest batch if it isn't full yet, otherwise start a new batch.
	if len(c.queue) == 0 || len(c.queue[len(c.queue)-1].subscribers) >= c.batchSize {
		c.queue = append(c.queue, batchedQuery{
			qb:          newQueryBuilder(),
			subscribers: map[string]*subscription{},
		})
	}
	batch := c.queue[len(c.queue)-1]

	respC := make(chan error, 1)

	sub := &subscription{
		ctx:   ctx,
		resp:  resp,
		respC: respC,
	}

	var vars map[string]any
	out, _ := json.Marshal(req.Variables)
	_ = json.Unmarshal(out, &vars)

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
			Enter: func(p visitor.VisitFuncParams) (string, interface{}) {
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

	for fragName := range qb.fragments {
		builder.WriteString("\n")
		builder.WriteString(_fragments[fragName])
	}

	return builder.String(), qb.vars, nil
}
