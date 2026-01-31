package internal

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestPathToID(t *testing.T) {
	tests := []struct {
		given   string
		want    int64
		wantErr error
	}{
		{
			given: "/book/show/27362503-it-ends-with-us",
			want:  27362503,
		},
		{
			given: "/book/show/7244.The_Poisonwood_Bible",
			want:  7244,
		},
		{
			given: "/work/1842237",
			want:  1842237,
		},
		{
			given: "/book/show/15704307-saga-volume-1",
			want:  15704307,
		},
		{
			given: "https://www.example.com/book/show/218467.Lucifer_s_Hammer",
			want:  218467,
		},
		{
			given: "/book/show/24035930-2",
			want:  24035930,
		},
		{
			given:   "/author/-1234",
			want:    -1234,
			wantErr: errBadRequest,
		},
		{
			given:   "/author/10000000000",
			want:    10000000000,
			wantErr: errBadRequest,
		},
	}

	for _, tt := range tests {
		actual, err := pathToID(tt.given)
		assert.ErrorIs(t, err, tt.wantErr)
		assert.Equal(t, tt.want, actual)
	}
}

func TestSearchBatchHandler(t *testing.T) {
	t.Parallel()

	c := gomock.NewController(t)
	getter := NewMockgetter(c)

	// Mock search results for different queries
	query1 := "test query 1"
	query2 := "test query 2"

	results1 := []SearchResource{
		{BookID: 1, WorkID: 10, Author: SearchResourceAuthor{ID: 100}},
	}
	results2 := []SearchResource{
		{BookID: 2, WorkID: 20, Author: SearchResourceAuthor{ID: 200}},
	}

	getter.EXPECT().Search(gomock.Any(), query1).Return(results1, nil).Times(1)
	getter.EXPECT().Search(gomock.Any(), query2).Return(results2, nil).Times(1)

	cache := newMemoryCache()
	ctrl, err := NewController(cache, getter, nil, nil)
	require.NoError(t, err)

	handler := NewHandler(ctrl)

	// Test the batch search endpoint
	queries := []string{query1, query2}
	body, _ := json.Marshal(queries)

	req := httptest.NewRequest(http.MethodPost, "/search/batch", bytes.NewReader(body))
	w := httptest.NewRecorder()

	handler.searchBatch(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var result BatchSearchResource
	err = json.Unmarshal(w.Body.Bytes(), &result)
	require.NoError(t, err)

	assert.Len(t, result.Results, 2)
	assert.Equal(t, results1, result.Results[query1])
	assert.Equal(t, results2, result.Results[query2])
}

