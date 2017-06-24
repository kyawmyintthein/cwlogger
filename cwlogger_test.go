package cwlogger

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"

	"io/ioutil"

	"regexp"

	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/stretchr/testify/assert"
)

func TestCreatesGroupAndStream(t *testing.T) {
	logGroupCreated := false
	logStreamCreated := false

	newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "CreateLogGroup" {
			var data CreateLogGroup
			parseBody(r, &data)
			assert.Equal(t, "test", data.LogGroupName)
			logGroupCreated = true
		}
		if action(r) == "CreateLogStream" {
			if !logGroupCreated {
				assert.Fail(t, "CreateLogGroup must be called before CreateLogStream")
			}
			var data CreateLogStream
			parseBody(r, &data)
			assert.Equal(t, "test", data.LogGroupName)
			assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}\.0$`), data.LogStreamName)
			logStreamCreated = true
		}
	})

	assert.True(t, logGroupCreated)
	assert.True(t, logStreamCreated)
}

func TestHandlesExistingGroup(t *testing.T) {
	logStreamCreated := false

	newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "CreateLogGroup" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`
				{
					"__type": "ResourceAlreadyExistsException",
					"message": "The specified log group already exists"
				}
			`))
		}
		if action(r) == "CreateLogStream" {
			logStreamCreated = true
		}
	})

	assert.True(t, logStreamCreated)
}

func TestSendsLogsToCloudWatchLogs(t *testing.T) {
	stg := new(SequenceTokenGenerator)
	var logStreamName string
	var req PutLogEvents

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "CreateLogStream" {
			var data CreateLogStream
			parseBody(r, &data)
			logStreamName = data.LogStreamName
		}
		if action(r) == "PutLogEvents" {
			parseBody(r, &req)
			stg.Write(w)
		}
	})

	logger.Log(time.Unix(1500000000, 0), "LOG MESSAGE")
	logger.Close()

	assert.Equal(t, "test", req.LogGroupName)
	assert.Equal(t, logStreamName, req.LogStreamName)
	assert.EqualValues(t, 1500000000000, req.LogEvents[0].Timestamp)
	assert.Equal(t, "LOG MESSAGE", req.LogEvents[0].Message)
	assert.Nil(t, req.SequenceToken)
}

func TestSequenceToken(t *testing.T) {
	stg := new(SequenceTokenGenerator)
	logChecker := NewLogChecker(1024)
	receivedSequenceTokens := []*string{}

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "PutLogEvents" {
			var data PutLogEvents
			parseBody(r, &data)
			receivedSequenceTokens = append(receivedSequenceTokens, data.SequenceToken)
			stg.Write(w)
		}
	})

	logChecker.Generate(logger, 3000)
	logger.Close()

	assert.Len(t, receivedSequenceTokens, 3)
	assert.Nil(t, receivedSequenceTokens[0])
	assert.Equal(t, "1", *receivedSequenceTokens[1])
	assert.Equal(t, "2", *receivedSequenceTokens[2])
}

func TestDataAlreadyAcceptedException(t *testing.T) {
	var (
		calls                 int
		receivedSequenceToken string
		logChecker            = NewLogChecker(1024)
	)

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "PutLogEvents" {
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`
					{
						"__type": "DataAlreadyAcceptedException",
						"expectedSequenceToken": "2"
					}
				`))
			} else {
				var data PutLogEvents
				parseBody(r, &data)
				receivedSequenceToken = *data.SequenceToken
				w.Write([]byte(`{"nextSequenceToken":"3"}`))
			}
		}
	})

	logChecker.Generate(logger, 2000)
	logger.Close()

	assert.Equal(t, 2, calls)
	assert.Equal(t, "2", receivedSequenceToken)
}

func TestInvalidSequenceTokenException(t *testing.T) {
	var (
		calls                 int
		receivedSequenceToken string
	)

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "PutLogEvents" {
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`
					{
						"__type": "InvalidSequenceTokenException",
						"expectedSequenceToken": "2"
					}
				`))
			} else {
				var data PutLogEvents
				parseBody(r, &data)
				receivedSequenceToken = *data.SequenceToken
				w.Write([]byte(`{"nextSequenceToken":"3"}`))
			}
		}
	})

	logger.Log(time.Now(), "message")
	logger.Close()

	assert.Equal(t, 2, calls)
	assert.Equal(t, "2", receivedSequenceToken)
}

func TestThrottlingException(t *testing.T) {
	stg := new(SequenceTokenGenerator)
	logChecker := NewLogChecker(1024)
	actions := []string{}
	var calls int

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		actions = append(actions, action(r))
		if action(r) == "PutLogEvents" {
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"__type":"ThrottlingException"}`))
			} else {
				var data PutLogEvents
				parseBody(r, &data)
				logChecker.Record(data.LogEvents)
				stg.Write(w)
			}
		}
	})

	logChecker.Generate(logger, 1000)
	logger.Close()

	assert.Equal(t,
		[]string{
			"CreateLogGroup", "CreateLogStream", "PutLogEvents", "CreateLogStream",
			"PutLogEvents",
		},
		actions)

	logChecker.Assert(t)
}

func TestConnectionFailure(t *testing.T) {
	stg := new(SequenceTokenGenerator)
	logChecker := NewLogChecker(1024)
	var calls int

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "PutLogEvents" {
			calls++
			if calls == 1 {
				hj, _ := w.(http.Hijacker)
				conn, _, _ := hj.Hijack()
				conn.Close()
			} else {
				var data PutLogEvents
				parseBody(r, &data)
				logChecker.Record(data.LogEvents)
				stg.Write(w)
			}
		}
	})

	logChecker.Generate(logger, 1000)
	logger.Close()

	logChecker.Assert(t)
}

func TestLogStreamCreationFailureAfterThrottlingException(t *testing.T) {
	stg := new(SequenceTokenGenerator)
	logChecker := NewLogChecker(1024)
	actions := []string{}
	var calls int

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		actions = append(actions, action(r))
		calls++
		if action(r) == "CreateLogStream" && calls == 4 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"__type":"ServiceUnavailableException"}`))
		}
		if action(r) == "PutLogEvents" {
			if calls == 3 || calls == 5 {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"__type":"ThrottlingException"}`))
			} else {
				var data PutLogEvents
				parseBody(r, &data)
				logChecker.Record(data.LogEvents)
				stg.Write(w)
			}
		}
	})

	logChecker.Generate(logger, 1000)
	logger.Close()

	assert.Equal(t,
		[]string{
			"CreateLogGroup", "CreateLogStream", "PutLogEvents", "CreateLogStream",
			"PutLogEvents", "CreateLogStream", "PutLogEvents"},
		actions)

	logChecker.Assert(t)
}

func TestInvalidJSONResponses(t *testing.T) {
	stg := new(SequenceTokenGenerator)
	logChecker := NewLogChecker(1024)
	var calls int

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "PutLogEvents" {
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("invalid"))
			} else if calls == 2 {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("invalid"))
			} else {
				var data PutLogEvents
				parseBody(r, &data)
				logChecker.Record(data.LogEvents)
				stg.Write(w)
			}
		}
	})

	logChecker.Generate(logger, 1000)
	logger.Close()

	logChecker.Assert(t)
}

func TestBatchByteSizeLimit(t *testing.T) {
	stg := new(SequenceTokenGenerator)
	logChecker := NewLogChecker(1024)

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "PutLogEvents" {
			var data PutLogEvents
			parseBody(r, &data)
			logChecker.Record(data.LogEvents)
			assert.Len(t, data.LogEvents, 1024)
			stg.Write(w)
		}
	})

	logChecker.Generate(logger, 3072)
	logger.Close()

	logChecker.Assert(t)
}

func TestBatchLengthLimit(t *testing.T) {
	stg := new(SequenceTokenGenerator)
	logChecker := NewLogChecker(55)

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "PutLogEvents" {
			var data PutLogEvents
			parseBody(r, &data)
			logChecker.Record(data.LogEvents)
			assert.Len(t, data.LogEvents, 10000)
			stg.Write(w)
		}
	})

	logChecker.Generate(logger, 30000)
	logger.Close()

	logChecker.Assert(t)
}

func TestBatchSendsDataAfterTimeout(t *testing.T) {
	stg := new(SequenceTokenGenerator)
	logChecker := NewLogChecker(1024)
	var wg sync.WaitGroup

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "PutLogEvents" {
			var data PutLogEvents
			parseBody(r, &data)
			logChecker.Record(data.LogEvents)
			stg.Write(w)
			wg.Done()
		}
	})

	wg.Add(2)
	logChecker.Generate(logger, 2000)
	wg.Wait()

	logChecker.Assert(t)
}

func TestLogGroupCreationFails(t *testing.T) {
	client := newClientWithServer(func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "CreateLogGroup" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"__type": "ServiceUnavailableException"}`))
		}
	})
	logger, err := NewLogGroup("test", client)
	assert.Error(t, err)
	assert.Nil(t, logger)
}

func TestLogStreamCreationFails(t *testing.T) {
	client := newClientWithServer(func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "CreateLogStream" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"__type": "ServiceUnavailableException"}`))
		}
	})
	logger, err := NewLogGroup("test", client)
	assert.Error(t, err)
	assert.Nil(t, logger)
}

func TestIgnoresBatchItCannotRetry(t *testing.T) {
	var calls int

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "PutLogEvents" {
			calls++
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"__type": "ResourceNotFoundException"}`))
		}
	})

	logger.Log(time.Now(), "message")
	logger.Close()

	assert.Equal(t, 1, calls)
}

func TestCustomErrorReporter(t *testing.T) {
	var calls int
	var errorMessages []string
	logChecker := NewLogChecker(1024)

	logger := newLogGroupWithServer("test", func(w http.ResponseWriter, r *http.Request) {
		if action(r) == "PutLogEvents" {
			calls++
			w.WriteHeader(http.StatusBadRequest)
			if calls == 1 {
				w.Write([]byte(`{"__type": "ResourceNotFoundException"}`))
			} else {
				w.Write([]byte(`
					{
						"__type": "UnknownError",
						"message": "unknown"
					}
				`))
			}
		}
	})

	logger.ErrorReporter = func(err error) {
		errorMessages = append(errorMessages, err.Error())
	}

	logChecker.Generate(logger, 2000)
	logger.Close()

	assert.Equal(t, "ResourceNotFoundException", errorMessages[0])
	assert.Equal(t, "UnknownError: unknown", errorMessages[1])
}

type CreateLogGroup struct {
	LogGroupName string `json:"logGroupName"`
}

type CreateLogStream struct {
	LogGroupName  string `json:"logGroupName"`
	LogStreamName string `json:"logStreamName"`
}

type PutLogEvents struct {
	LogGroupName  string      `json:"logGroupName"`
	LogStreamName string      `json:"logStreamName"`
	SequenceToken *string     `json:"sequenceToken"`
	LogEvents     []*LogEvent `json:"logEvents"`
}

type LogEvent struct {
	Timestamp int64  `json:"timestamp"`
	Message   string `json:"message"`
}

func newClientWithServer(handler http.HandlerFunc) *cloudwatchlogs.CloudWatchLogs {
	server := httptest.NewServer(http.HandlerFunc(handler))
	session := session.New()
	config := aws.NewConfig().
		WithMaxRetries(0).
		WithEndpoint(server.URL).
		WithRegion("us-east-1").
		WithCredentials(credentials.NewStaticCredentials("id", "secret", "token"))
	return cloudwatchlogs.New(session, config)
}

func newLogGroupWithServer(name string, handler http.HandlerFunc) *LogGroup {
	client := newClientWithServer(handler)
	logGroup, err := NewLogGroup(name, client)
	if err != nil {
		panic(err)
	}
	return logGroup
}

func action(r *http.Request) string {
	return strings.Split(r.Header.Get("X-Amz-Target"), ".")[1]
}

func parseBody(r *http.Request, target interface{}) {
	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		panic(err)
	}
	json.Unmarshal(b, target)
}

type SequenceTokenGenerator struct {
	token int
}

func (stg *SequenceTokenGenerator) Next() string {
	stg.token++
	return strconv.Itoa(stg.token)
}

func (stg *SequenceTokenGenerator) Write(w http.ResponseWriter) {
	w.Write([]byte(`{"nextSequenceToken":"` + stg.Next() + `"}`))
}

type TestLogMessage struct {
	ID   string
	Data string
}

type TestLogMessages []*TestLogMessage

func (m TestLogMessages) Len() int {
	return len(m)
}

func (m TestLogMessages) Less(i, j int) bool {
	a, err := strconv.Atoi(m[i].ID)
	if err != nil {
		panic(err)
	}
	b, err := strconv.Atoi(m[j].ID)
	if err != nil {
		panic(err)
	}
	return a < b
}

func (m TestLogMessages) Swap(i, j int) {
	m[i], m[j] = m[j], m[i]
}

type LogChecker struct {
	id        int
	data      string
	generated TestLogMessages
	recorded  TestLogMessages
}

func NewLogChecker(messageSize int) *LogChecker {
	if messageSize < 55 {
		panic("message size must be at least 55 bytes")
	}
	var data string
	for i := 0; i < (messageSize - 54); i++ {
		data += "."
	}
	return &LogChecker{
		data:      data,
		generated: make(TestLogMessages, 0),
		recorded:  make(TestLogMessages, 0),
	}
}

func (c *LogChecker) Generate(lg *LogGroup, count int) {
	for i := 0; i < count; i++ {
		c.id++
		message := &TestLogMessage{
			ID:   fmt.Sprintf("%09d", c.id),
			Data: c.data,
		}
		c.generated = append(c.generated, message)
		jsonMessage, err := json.Marshal(message)
		if err != nil {
			panic(err)
		}
		lg.Log(time.Now(), string(jsonMessage))
	}
}

func (c *LogChecker) Record(events []*LogEvent) {
	for _, event := range events {
		var message TestLogMessage
		err := json.Unmarshal([]byte(event.Message), &message)
		if err != nil {
			panic(err)
		}
		c.recorded = append(c.recorded, &message)
	}
}

func (c *LogChecker) Assert(t *testing.T) {
	sort.Sort(c.generated)
	sort.Sort(c.recorded)
	assert.Equal(t, c.generated, c.recorded)
}