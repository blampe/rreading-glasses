package internal

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/blampe/rreading-glasses/hardcover"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

func TestQueryBuilder(t *testing.T) {
	qb := newQueryBuilder()

	query1 := hardcover.GetBook_Operation
	vars1 := map[string]interface{}{"grBookIDs": []string{"1"}}

	query2 := hardcover.GetAuthorEditions_Operation
	vars2 := map[string]interface{}{
		"id":     1,
		"limit":  2,
		"offset": 3,
	}

	id1, _, err := qb.add(query1, vars1)
	require.NoError(t, err)

	id2, _, err := qb.add(query2, vars2)
	require.NoError(t, err)

	query, vars, err := qb.build()
	require.NoError(t, err)

	expected := fmt.Sprintf(`query GetBook($%s_grBookID: String!, $%s_id: Int!, $%s_limit: Int!, $%s_offset: Int!) {
  %s: book_mappings(limit: 1, where: {platform_id: {_eq: 1}, external_id: {_eq: $%s_grBookID}}) {
    external_id
    edition {
      id
      title
      subtitle
      asin
      isbn_13
      edition_format
      pages
      audio_seconds
      language {
        language
      }
      publisher {
        name
      }
      release_date
      description
      identifiers
      book_id
    }
    book {
      id
      title
      subtitle
      description
      release_date
      cached_tags(path: "$.Genre")
      cached_image(path: "url")
      contributions {
        contributable_type
        contribution
        author {
          id
          name
          slug
          bio
          cached_image(path: "url")
        }
      }
      slug
      book_series {
        position
        series {
          id
          name
          description
          identifiers
        }
      }
      book_mappings {
        dto_external
      }
      rating
      ratings_count
    }
  }
  %s: authors(limit: 1, where: {id: {_eq: $%s_id}}) {
    location
    id
    slug
    contributions(limit: $%s_limit, offset: $%s_offset, order_by: {id: asc}, where: {contributable_type: {_eq: "Book"}}) {
      book {
        id
        title
        ratings_count
        book_mappings(limit: 1, where: {platform_id: {_eq: 1}}) {
          book_id
          edition_id
          external_id
        }
      }
    }
    identifiers(path: "goodreads[0]")
  }
}`, id1, id2, id2, id2, id1, id1, id2, id2, id2, id2)

	assert.Equal(t, expected, query)

	assert.Len(t, vars, 4)
	assert.Contains(t, vars, id1+"_grBookID", id2+"_id", id2+"_limit", id2+"_offset")
}

func TestBatching(t *testing.T) {
	apiKey := os.Getenv("HARDCOVER_API_KEY")
	if apiKey == "" {
		t.Skip("missing HARDCOVER_API_KEY")
		return
	}
	transport := HeaderTransport{
		Key:          "Authorization",
		Value:        "Bearer " + apiKey,
		RoundTripper: http.DefaultTransport,
	}

	client := &http.Client{Transport: transport}

	url := "https://api.hardcover.app/v1/graphql"

	gql, err := NewBatchedGraphQLClient(url, client, time.Second)
	require.NoError(t, err)

	start := time.Now()

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := hardcover.GetBook(context.Background(), gql, "0156028352")
		if err != nil {
			panic(err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := hardcover.GetBook(context.Background(), gql, "0164005178")
		if err != nil {
			panic(err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := hardcover.GetBook(context.Background(), gql, "0340640138")
		if err != nil {
			panic(err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := hardcover.GetBook(context.Background(), gql, "missing")
		if err != nil {
			panic(err)
		}
	}()

	wg.Wait()

	assert.Less(t, time.Since(start), 4*time.Second)
}

func TestGQLStatusCode(t *testing.T) {
	err := &gqlerror.Error{Message: "womp"}
	assert.ErrorIs(t, err, gqlStatusErr(err))

	err = &gqlerror.Error{Message: "Request failed with status code 403"}
	err403 := statusErr(403)
	assert.ErrorAs(t, gqlStatusErr(err), &err403)
}
