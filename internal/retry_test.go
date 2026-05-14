package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGQLClient lets each test script a sequence of errors-then-success.
type fakeGQLClient struct {
	errs    []error
	n       atomic.Int32
	delay   time.Duration // sleep before returning, simulates work
	respect bool          // honor ctx cancellation in the delay
}

func (f *fakeGQLClient) MakeRequest(ctx context.Context, _ *graphql.Request, _ *graphql.Response) error {
	if f.delay > 0 {
		if f.respect {
			select {
			case <-time.After(f.delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		} else {
			time.Sleep(f.delay)
		}
	}
	i := f.n.Add(1) - 1
	if int(i) >= len(f.errs) {
		return nil
	}
	return f.errs[i]
}

func TestIsTransientNetErr(t *testing.T) {
	// Synthesize the kind of error a real http.Client returns on a dial
	// timeout: *url.Error wrapping *net.OpError wrapping a syscall.Errno.
	wrappedDialTimeout := &url.Error{
		Op:  "Post",
		URL: "https://api.example/graphql",
		Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: syscall.ETIMEDOUT,
		},
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("oh no"), false},
		{"plain wrapped error", fmt.Errorf("doing thing: %w", errors.New("oh no")), false},

		// Context — caller cancellation / per-attempt deadline.
		{"context deadline", context.DeadlineExceeded, true},
		{"context canceled", context.Canceled, true},
		{"wrapped context deadline", fmt.Errorf("getting: %w", context.DeadlineExceeded), true},

		// Premature EOF mid-response.
		{"io EOF", io.EOF, true},
		{"io unexpected EOF", io.ErrUnexpectedEOF, true},
		{"wrapped EOF", fmt.Errorf("reading: %w", io.EOF), true},

		// Socket-level syscalls (typed, not stringly).
		{"ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"ECONNRESET", syscall.ECONNRESET, true},
		{"ECONNABORTED", syscall.ECONNABORTED, true},
		{"EPIPE", syscall.EPIPE, true},
		{"ETIMEDOUT", syscall.ETIMEDOUT, true},
		{"EHOSTUNREACH", syscall.EHOSTUNREACH, true},
		{"ENETUNREACH", syscall.ENETUNREACH, true},

		// DNS — retry unless permanently unresolvable.
		{"DNS not found", &net.DNSError{Name: "nope.invalid", IsNotFound: true}, false},
		{"DNS timeout", &net.DNSError{Name: "foo.example", IsTimeout: true}, true},
		{"DNS temporary", &net.DNSError{Name: "foo.example", IsTemporary: true}, true},
		{"DNS generic", &net.DNSError{Name: "foo.example"}, true},

		// http.Client wraps everything in *url.Error.
		{"url.Error wrapping dial timeout", wrappedDialTimeout, true},
		{"url.Error wrapping non-net", &url.Error{Op: "Post", URL: "https://x", Err: errors.New("malformed")}, false},

		// net.OpError catch-all.
		{"net.OpError dial generic", &net.OpError{Op: "dial", Err: errors.New("unreachable")}, true},

		// Response-level errors — alive upstream, do NOT retry.
		{"http 500 in message", errors.New("server returned 500 Internal Server Error"), false},
		{"genqlient status 400", errors.New("Request failed with status code 400"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isTransientNetErr(c.err))
		})
	}
}

func TestRetryMakeRequest_SucceedsOnFirstTry(t *testing.T) {
	f := &fakeGQLClient{errs: nil}
	err := retryMakeRequest(context.Background(), f, &graphql.Request{}, &graphql.Response{})
	require.NoError(t, err)
	assert.Equal(t, int32(1), f.n.Load())
}

func TestRetryMakeRequest_RetriesAndSucceeds(t *testing.T) {
	timeout := &net.OpError{Op: "dial", Err: syscall.ETIMEDOUT}
	f := &fakeGQLClient{errs: []error{timeout, timeout}}
	start := time.Now()
	err := retryMakeRequest(context.Background(), f, &graphql.Request{}, &graphql.Response{})
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Equal(t, int32(3), f.n.Load())
	// 1s + 2s = 3s of base backoff between the 3 calls; jitter adds up to
	// (1s + 2s) / jitterFraction = 750ms extra in the worst case.
	assert.GreaterOrEqual(t, elapsed, 3*time.Second)
	assert.Less(t, elapsed, 4500*time.Millisecond)
}

func TestRetryMakeRequest_DoesNotRetryOnNonTransient(t *testing.T) {
	f := &fakeGQLClient{errs: []error{errors.New("bad request body")}}
	err := retryMakeRequest(context.Background(), f, &graphql.Request{}, &graphql.Response{})
	require.Error(t, err)
	assert.Equal(t, int32(1), f.n.Load()) // exactly one attempt, no retry
}

func TestRetryMakeRequest_DoesNotRetryOnDNSNotFound(t *testing.T) {
	notFound := &net.DNSError{Name: "nope.invalid", IsNotFound: true}
	f := &fakeGQLClient{errs: []error{notFound}}
	err := retryMakeRequest(context.Background(), f, &graphql.Request{}, &graphql.Response{})
	require.Error(t, err)
	assert.Equal(t, int32(1), f.n.Load())
}

func TestRetryMakeRequest_GivesUpAfterMaxAttempts(t *testing.T) {
	transient := syscall.ECONNREFUSED
	f := &fakeGQLClient{errs: []error{transient, transient, transient, transient}}
	err := retryMakeRequest(context.Background(), f, &graphql.Request{}, &graphql.Response{})
	require.Error(t, err)
	assert.Equal(t, int32(maxRetryAttempts), f.n.Load())
	assert.ErrorIs(t, err, syscall.ECONNREFUSED)
}

func TestRetryMakeRequest_AbortsOnContextCancel(t *testing.T) {
	transient := syscall.ETIMEDOUT
	f := &fakeGQLClient{errs: []error{transient, transient, transient}}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := retryMakeRequest(ctx, f, &graphql.Request{}, &graphql.Response{})
	elapsed := time.Since(start)
	require.Error(t, err)
	// Should bail out well before the 1+2+4=7s of full backoff.
	assert.Less(t, elapsed, 2*time.Second, fmt.Sprintf("aborted in %v", elapsed))
}
