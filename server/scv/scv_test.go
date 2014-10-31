package scv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"../util"
)

var _ = fmt.Printf

var serverAddr string = "http://127.0.0.1/streams/wowsogood"

type Fixture struct {
	app *Application
}

func (f *Fixture) addUser(user string) (token string) {
	token = util.RandSeq(36)
	type Msg struct {
		Id    string `bson:"_id"`
		Token string `bson:"token"`
	}
	f.app.Mongo.DB("users").C("all").Insert(Msg{user, token})
	return
}

func (f *Fixture) addManager(user string, weight int) (token string) {
	token = f.addUser(user)
	type Msg struct {
		Id     string `bson:"_id"`
		Weight int    `bson:"weight"`
	}
	f.app.Mongo.DB("users").C("managers").Insert(Msg{user, weight})
	return
}

func NewFixture() *Fixture {
	config := Configuration{
		MongoURI:     "localhost:27017",
		Name:         "testServer",
		Password:     "hello",
		ExternalHost: "alexis.stanford.edu",
		InternalHost: "127.0.0.1",
	}
	f := Fixture{
		app: NewApplication(config),
	}
	db_names, _ := f.app.Mongo.DatabaseNames()
	for _, name := range db_names {
		f.app.Mongo.DB(name).DropDatabase()
	}
	return &f
}

func (f *Fixture) shutdown() {
	db_names, _ := f.app.Mongo.DatabaseNames()
	for _, name := range db_names {
		f.app.Mongo.DB(name).DropDatabase()
	}
	os.RemoveAll(f.app.Config.Name + "_data")
	f.app.Shutdown()
}

// func TestPostStreamUnauthorized(t *testing.T) {
// 	f := NewFixture()
// 	defer f.shutdown()
// 	req, _ := http.NewRequest("POST", "/streams", nil)
// 	w := httptest.NewRecorder()
// 	f.app.Router.ServeHTTP(w, req)
// 	assert.Equal(t, w.Code, 401)
// 	token := f.addUser("yutong")
// 	req, _ = http.NewRequest("POST", "/streams", nil)
// 	req.Header.Add("Authorization", token)
// 	w = httptest.NewRecorder()

// 	f.app.Router.ServeHTTP(w, req)

// 	//app.PostStreamHandler().ServeHTTP(w, req)
// 	assert.Equal(t, w.Code, 401)
// }

// func TestPostBadStream(t *testing.T) {
// 	f := NewFixture()
// 	defer f.shutdown()
// 	token := f.addManager("yutong", 1)

// 	jsonData := `{"target_id":"12345", "files": {"openmm": "ZmlsZWRhdG`
// 	dataBuffer := bytes.NewBuffer([]byte(jsonData))
// 	req, _ := http.NewRequest("POST", "/streams", dataBuffer)
// 	req.Header.Add("Authorization", token)
// 	w := httptest.NewRecorder()
// 	f.app.Router.ServeHTTP(w, req)
// 	assert.Equal(t, w.Code, 400)
// }

func (f *Fixture) activateStream(target_id, engine, user, cc_token string) (token string, code int) {
	type Message struct {
		TargetId string `json:"target_id"`
		Engine   string `json:"engine"`
		User     string `json:"user"`
	}
	msg := Message{target_id, engine, user}
	data, _ := json.Marshal(msg)
	req, _ := http.NewRequest("POST", "/streams/activate", bytes.NewBuffer(data))
	req.Header.Add("Authorization", cc_token)
	w := httptest.NewRecorder()
	f.app.Router.ServeHTTP(w, req)
	code = w.Code
	if code != 200 {
		return
	}
	result := make(map[string]string)
	json.Unmarshal(w.Body.Bytes(), &result)
	token = result["token"]
	return
}

func (f *Fixture) getStream(stream_id string) (result Stream, code int) {
	req, _ := http.NewRequest("GET", "/streams/info/"+stream_id, nil)
	w := httptest.NewRecorder()
	f.app.Router.ServeHTTP(w, req)
	json.Unmarshal(w.Body.Bytes(), &result)
	code = w.Code
	return
}

func (f *Fixture) postStream(token string, data string) (stream_id string, code int) {
	dataBuffer := bytes.NewBuffer([]byte(data))
	req, _ := http.NewRequest("POST", "/streams", dataBuffer)
	req.Header.Add("Authorization", token)
	w := httptest.NewRecorder()
	f.app.Router.ServeHTTP(w, req)
	code = w.Code
	if code != 200 {
		return
	}
	stream_map := make(map[string]string)
	json.Unmarshal(w.Body.Bytes(), &stream_map)
	stream_id = stream_map["stream_id"]
	return
}

func TestPostStream(t *testing.T) {
	f := NewFixture()
	defer f.shutdown()
	token := f.addManager("yutong", 1)
	start := int(time.Now().Unix())
	jsonData := `{"target_id":"12345",
		"files": {"openmm": "ZmlsZWRhdGFibGFoYmFsaA==",
		"amber": "ZmlsZWRhdGFibGFoYmFsaA=="}}`
	stream_id, code := f.postStream(token, jsonData)
	assert.Equal(t, code, 200)
	mStream, code := f.getStream(stream_id)

	assert.Equal(t, code, 200)
	assert.Equal(t, "OK", mStream.Status)
	assert.Equal(t, 0, mStream.Frames)
	assert.Equal(t, 0, mStream.ErrorCount)
	assert.True(t, mStream.CreationDate-start < 1)

	_, code = f.getStream("12345")
	assert.Equal(t, code, 400)

	// try adding tags
	jsonData = `{"target_id":"12345",
	    "files": {"openmm": "ZmlsZWRhdGFibGFoYmFsaA==", "amber": "ZmlsZWRhdGFibGFoYmFsaA=="},
		"tags": {"openmm": "ZmlsZWRhdGFibGFoYmFsaA=="}}`
	stream_id, code = f.postStream(token, jsonData)
	assert.Equal(t, code, 200)
}

func TestPostStreamAsync(t *testing.T) {
	f := NewFixture()
	defer f.shutdown()
	token := f.addManager("yutong", 1)
	start := int(time.Now().Unix())
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			jsonData := `{"target_id":"12345",
				"files": {"openmm": "ZmlsZWRhdGFibGFoYmFsaA==",
				"amber": "ZmlsZWRhdGFibGFoYmFsaA=="}}`
			stream_id, code := f.postStream(token, jsonData)
			assert.Equal(t, code, 200)
			mStream, code := f.getStream(stream_id)
			assert.Equal(t, code, 200)
			assert.Equal(t, "OK", mStream.Status)
			assert.Equal(t, 0, mStream.Frames)
			assert.Equal(t, 0, mStream.ErrorCount)
			assert.True(t, mStream.CreationDate-start < 1)
			wg.Done()
		}()
	}
	wg.Wait()
}

// func TestFaultyStreamActivation(t *testing.T) {
// 	f := NewFixture()
// 	defer f.shutdown()
// 	token := f.addManager("yutong", 1)
// 	var mu sync.Mutex
// 	stream_ids := make([]string, 10, 10)
// 	var wg sync.WaitGroup
// 	target_id := "123456"
// 	for i := 0; i < 10; i++ {
// 		wg.Add(1)
// 		go func() {
// 			jsonData := `{"target_id":"` + target_id + `",
// 				"files": {"openmm": "ZmlsZWRhdGFibGFoYmFsaA==",
// 				"amber": "ZmlsZWRhdGFibGFoYmFsaA=="}}`
// 			stream_id, code := f.postStream(token, jsonData)
// 			mu.Lock()
// 			stream_ids = append(stream_ids, stream_id)
// 			mu.Unlock()
// 			assert.Equal(t, code, 200)
// 			wg.Done()
// 		}()
// 	}
// 	wg.Wait()
// 	_, code := f.activateStream(target_id, "a", "b", "bad_pass")
// 	assert.Equal(t, code, 401)
// 	_, code = f.activateStream("54321", "a", "b", f.app.Config.Password)
// 	assert.Equal(t, code, 400)
// }

func TestStreamActivation(t *testing.T) {
	f := NewFixture()
	defer f.shutdown()
	token := f.addManager("yutong", 1)
	var mu sync.Mutex
	stream_ids := make(map[string]struct{})
	var wg sync.WaitGroup
	target_id := "123456"
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			jsonData := `{"target_id":"` + target_id + `",
				"files": {"openmm": "ZmlsZWRhdGFibGFoYmFsaA==",
				"amber": "ZmlsZWRhdGFibGFoYmFsaA=="}}`
			stream_id, code := f.postStream(token, jsonData)
			mu.Lock()
			stream_ids[stream_id] = struct{}{}
			mu.Unlock()
			assert.Equal(t, code, 200)
			wg.Done()
		}()
	}
	wg.Wait()

	tokens := make(map[string]struct{})

	// activate 10 times asynchronously
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			engine := util.RandSeq(12)
			user := util.RandSeq(12)
			token, code := f.activateStream(target_id, engine, user, f.app.Config.Password)
			assert.Equal(t, code, 200)
			tokens[token] = struct{}{}
			wg.Done()
		}()
	}
	wg.Wait()
	_, code := f.activateStream(target_id, "random", "guy", f.app.Config.Password)
	assert.Equal(t, code, 400)
}
