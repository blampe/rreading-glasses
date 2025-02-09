package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Khan/genqlient/graphql"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// gqlclient wraps graphql.Client and translates errors into meaningful status
// codes. The client normally returns error responses with a 200 OK status code
// and a populated "Errors" field containing stringed errors. We want to
// instead surface e.g. 404 errors directly.
type gqlclient struct {
	wrapped graphql.Client
}

// MakeRequest satisfied graphql.Client.
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
