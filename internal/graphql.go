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
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/printer"
	"github.com/graphql-go/graphql/language/source"
	"github.com/graphql-go/graphql/language/visitor"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"golang.org/x/exp/rand"
)

// batchedgqlclient accumulates queries and executes them in batch in order to
// make better use of RPS limits.
type batchedgqlclient struct {
	mu sync.Mutex

	subscriptions map[string]*subscription
	qb            *queryBuilder

	wrapped graphql.Client
}

// NewBatchedGraphQLClient creates a batching GraphQL client. Queries are
// accumulated and executed regularly accurding to the given rate.
func NewBatchedGraphQLClient(url string, client *http.Client, rate time.Duration) (graphql.Client, error) {
	wrapped, err := newGraphqlClient(url, client)
	if err != nil {
		return nil, err
	}
	c := &batchedgqlclient{
		qb:            newQueryBuilder(),
		subscriptions: map[string]*subscription{},
		wrapped:       wrapped,
	}

	go func() {
		for {
			time.Sleep(rate)
			c.flush(context.Background())
		}
	}()
	return c, nil
}

// flush executes the aggregated queries and returns responses to listeners.
func (c *batchedgqlclient) flush(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.qb.op == nil {
		return // Nothing to do yet.
	}

	query, vars, err := c.qb.build()
	if err != nil {
		Log(ctx).Error("unable to build query", "err", err)
		return
	}

	data := map[string]any{}
	req := &graphql.Request{
		Query:     query,
		Variables: vars,
		OpName:    c.qb.op.Name.Value,
	}
	resp := &graphql.Response{
		Data: &data,
	}

	// Hold on to our subscribers before we reset the batcher.
	subscriptions := c.subscriptions

	// Issue the request in a separate goroutine so we can continue to
	// accumulate queries without needing to wait for the network call. err =
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()

		err := c.wrapped.MakeRequest(ctx, req, resp)
		if err != nil {
			Log(ctx).Warn("batched query error", "count", c.qb.fields, "err", err)
			for _, sub := range subscriptions {
				sub.respC <- err
			}
			return
		}

		for id, sub := range subscriptions {
			// TODO: missing response.
			byt, err := json.Marshal(map[string]any{
				sub.field: data[id],
			})
			if err != nil {
				sub.respC <- err
				continue
			}

			sub.respC <- json.Unmarshal(byt, &sub.resp.Data) // TODO: Need an actual error here.
		}
	}()

	c.qb = newQueryBuilder()
	c.subscriptions = map[string]*subscription{}
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

	respC := make(chan error, 1)

	sub := &subscription{
		ctx:   ctx,
		resp:  resp,
		respC: respC,
	}

	var vars map[string]any
	out, _ := json.Marshal(req.Variables)
	_ = json.Unmarshal(out, &vars)

	id, field, err := c.qb.add(req.Query, vars)
	if err != nil {
		respC <- err
	}

	c.subscriptions[id] = &subscription{
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

// gqlclient wraps graphql.Client and translates errors into meaningful status
// codes. The client normally returns error responses with a 200 OK status code
// and a populated "Errors" field containing stringed errors. We want to
// instead surface e.g. 404 errors directly.
type gqlclient struct {
	wrapped graphql.Client
}

// MakeRequest implements graphql.Client.
func (c gqlclient) MakeRequest(
	ctx context.Context,
	req *graphql.Request,
	resp *graphql.Response,
) error {
	err := c.wrapped.MakeRequest(ctx, req, resp)
	var elist gqlerror.List
	if err == nil || !errors.As(err, &elist) {
		return err
	}

	// Unwrap the error list and find any status code errors so we can return
	// them to the client.
	err = nil
	for _, e := range elist.Unwrap() {
		errStr := e.Error()
		idx := strings.Index(errStr, "Request failed with status code")
		if idx == -1 {
			err = errors.Join(err, e)
			continue
		}
		code, _ := pathToID(errStr[idx:])
		err = errors.Join(err, statusErr(code))
	}
	return err
}

func newGraphqlClient(url string, client *http.Client) (graphql.Client, error) {
	c := graphql.NewClient(url, client)
	return gqlclient{c}, nil
}

// queryBuilder accumulates queries into one query with multiple fields so they
// can all be executed as part of one request.
type queryBuilder struct {
	op     *ast.OperationDefinition
	fields int
	vars   map[string]interface{}
}

// newQueryBuilder initializes a new QueryBuilder with an empty Document.
func newQueryBuilder() *queryBuilder {
	return &queryBuilder{
		vars: make(map[string]interface{}),
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
func (qb *queryBuilder) add(query string, vars map[string]interface{}) (id string, field string, err error) {
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

		qb.fields++

		if qb.op == opDef {
			continue
		}

		qb.op.SelectionSet.Selections = append(qb.op.SelectionSet.Selections, opDef.SelectionSet.Selections...)
		qb.op.VariableDefinitions = append(qb.op.VariableDefinitions, opDef.VariableDefinitions...)
	}

	return id, field, nil
}

// Build returns the merged query string and variables map.
func (qb *queryBuilder) build() (string, map[string]interface{}, error) {
	queryStr := printer.Print(qb.op)
	return fmt.Sprint(queryStr), qb.vars, nil
}
