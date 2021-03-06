package scv

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

var _ = fmt.Printf

type Application struct {
	Config  Configuration
	Mongo   *mgo.Session
	Manager *Manager
	Router  *mux.Router

	server     *Server
	stats      *list.List // things we put in this list should persist when server dies
	statsWG    sync.WaitGroup
	statsMutex sync.Mutex
	shutdown   chan os.Signal
	finish     chan struct{}
}

/*
Deactivate a stream and record its statistics. It is expected that mutexes are held for both
the manager and stream. Note that this methods runs very fast. Mongo operations are moved to a queue
and processed by a separate goroutine.
*/
func (app *Application) DeactivateStreamService(s *Stream) error {
	// Record stats for stream and defer insertion until later.
	stats := make(map[string]interface{})
	streamId := s.StreamId
	donorFrames := s.activeStream.donorFrames
	stats["engine"] = s.activeStream.engine
	stats["user"] = s.activeStream.user
	stats["start_time"] = s.activeStream.startTime
	stats["end_time"] = int(time.Now().Unix())
	stats["frames"] = donorFrames
	stats["stream"] = streamId
	stats_cursor := app.Mongo.DB("stats").C(s.TargetId)
	// Record statistics for the stream.
	fn1 := func() error {
		return stats_cursor.Insert(stats)
	}
	// Update the stream's frames, error_count, and status in Mongo
	status := "enabled"
	if s.ErrorCount >= MAX_STREAM_FAILS {
		status = "disabled"
	}
	stream_prop := bson.M{"$set": bson.M{"frames": s.Frames, "error_count": s.ErrorCount, "status": status}}
	stream_cursor := app.Mongo.DB("streams").C(app.Config.Name)
	fn2 := func() error {
		// Generally, if the error_count or the status fails to update, it's not a catastrophic error. We
		// can get away with a slightly dirty state for error_count and status if necessary.
		stream_cursor.UpdateId(streamId, stream_prop)
		return nil
	}

	app.statsMutex.Lock()
	if donorFrames > 0 {
		app.stats.PushBack(fn1)
	}
	app.stats.PushBack(fn2)
	app.statsMutex.Unlock()
	return nil
}

// Implements interface method for Manager's Injector. Only the stream is locked, manager is not.
func (app *Application) EnableStreamService(s *Stream) error {
	cursor := app.Mongo.DB("streams").C(app.Config.Name)
	s.ErrorCount = 0
	s.MongoStatus = "enabled"
	return cursor.UpdateId(s.StreamId, bson.M{"$set": bson.M{"status": "enabled", "error_count": 0}})
}

// Implements interface method for Manager's Injector. Only the stream is locked, manager is not.
func (app *Application) DisableStreamService(s *Stream) error {
	cursor := app.Mongo.DB("streams").C(app.Config.Name)
	// fmt.Println("DISABLING STREAM", streamId)
	return cursor.UpdateId(s.StreamId, bson.M{"$set": bson.M{"status": "disabled"}})
}

// app.stats contains a list of Mongo functions to be executed. Breaks if the function failed.
func (app *Application) drainStats() {
	app.statsMutex.Lock()
	for app.stats.Len() > 0 {
		ele := app.stats.Front()
		fn := ele.Value.(func() error)
		err := fn()
		if err == nil {
			app.stats.Remove(ele)
		} else {
			fmt.Println(err)
			break
		}
	}
	app.statsMutex.Unlock()
}

// A separate goroutine that populates MongoDB with stats entries.
func (app *Application) RecordDeferredDocs() {
	defer app.statsWG.Done()
	for {
		select {
		case <-app.finish:
			app.drainStats()
			// TOOD: persist stats here if not empty
			// if app.stats.Len() > 0 {
			// }
			return
		default:
			app.drainStats()
			time.Sleep(1 * time.Second)
		}
	}
}

type Configuration struct {
	MongoURI     string            `json:"MongoURI" bson:"-"`
	Name         string            `json:"Name" bson:"_id"`
	Password     string            `json:"Password" bson:"password"`
	ExternalHost string            `json:"ExternalHost" bson:"host"`
	InternalHost string            `json:"InternalHost" bson:"-"`
	SSL          map[string]string `json:"SSL" bson:"-"`
}

// Registers the SCV with MongoDB
func (app *Application) RegisterSCV() {
	log.Printf("Registering SCV %s with database...", app.Config.Name)
	cursor := app.Mongo.DB("servers").C("scvs")
	_, err := cursor.UpsertId(app.Config.Name, app.Config)
	if err != nil {
		panic("Could not connect to MongoDB: " + err.Error())
	}
}

/*
Invoked on start of the SCV. The following happens:
1. Loads the list of streams from Mongo. It is guaranteed that if a stream exists in Mongo, then it must exist on disk.
2. Any stream that is on the disk but not in Mongo is removed.
3. The status of the stream (enabled, disabled) is set.
4. If the frame count on disk (as determined by the folders available) is the canonical value. If it does not match
   the value inside MongoDB, then frame count value inside Mongo is then updated.
*/
func (app *Application) LoadStreams() {
	var mongoStreams []Stream

	err := app.StreamsCursor().Find(bson.M{}).All(&mongoStreams)
	if err != nil {
		panic("Could not connect to MongoDB: " + err.Error())
	}

	mongoStreamIds := make(map[string]Stream)
	for _, val := range mongoStreams {
		mongoStreamIds[val.StreamId] = val
	}

	log.Printf("Loading %d streams...", len(mongoStreamIds))

	diskStreamIds := make(map[string]struct{})
	fileData, err := ioutil.ReadDir(filepath.Join(app.Config.Name+"_data", "streams"))
	for _, v := range fileData {
		diskStreamIds[v.Name()] = struct{}{}
	}
	// Check that disk streams is equal to mongo streams. That is mongoStreams /subset of diskStreamIds
	for streamId, stream := range mongoStreamIds {
		_, ok := diskStreamIds[streamId]
		if ok == false {
			log.Panicln("Cannot find data for stream " + streamId + " on disk")
		}
		partitions, err := app.ListPartitions(streamId)
		if err != nil {
			panic("Unable to list partitions for stream " + streamId)
		}
		lastFrame := 0
		if len(partitions) > 0 {
			lastFrame = partitions[len(partitions)-1]
		}
		if lastFrame != stream.Frames {
			log.Printf("Warning: frame count mismatch for stream %s. Disk: %d, Mongo: %d, using disk value.", streamId, lastFrame, stream.Frames)
		}
		stream.Frames = lastFrame
		mongoStreamIds[streamId] = stream
	}
	for streamId, _ := range diskStreamIds {
		_, ok := mongoStreamIds[streamId]
		if ok == false {
			streamDir := app.StreamDir(streamId)
			log.Println("Warning: stream " + streamId + " is present on disk but not in Mongo, removing " + streamDir)
			os.RemoveAll(streamDir)
		}
	}

	for _, stream := range mongoStreamIds {
		stream_copy := stream
		if stream.MongoStatus == "enabled" {
			app.Manager.AddStream(&stream_copy, stream.TargetId, true)
		} else if stream.MongoStatus == "disabled" {
			app.Manager.AddStream(&stream_copy, stream.TargetId, false)
		} else {
			panic("Unknown stream status")
		}
	}
}

func NewApplication(config Configuration) *Application {
	session, err := mgo.Dial(config.MongoURI)
	if err != nil {
		panic(err)
	}
	app := Application{
		Config:  config,
		Mongo:   session,
		Manager: nil,
		stats:   list.New(),
		finish:  make(chan struct{}),
	}

	index := mgo.Index{
		Key:        []string{"target_id"},
		Background: true,
	}
	app.StreamsCursor().EnsureIndex(index)

	app.Manager = NewManager(&app)
	app.Router = mux.NewRouter()
	app.Router.Handle("/", app.AliveHandler()).Methods("GET")
	app.Router.Handle("/active_streams", app.ActiveStreamsHandler()).Methods("GET")
	app.Router.Handle("/streams", app.StreamsHandler()).Methods("POST")
	app.Router.Handle("/streams/info/{stream_id}", app.StreamInfoHandler()).Methods("GET")
	app.Router.Handle("/streams/activate", app.StreamActivateHandler()).Methods("POST")
	app.Router.Handle("/streams/download/{stream_id}/{file:.+}", app.StreamDownloadHandler()).Methods("GET")
	app.Router.Handle("/streams/start/{stream_id}", app.StreamEnableHandler()).Methods("PUT")
	app.Router.Handle("/streams/stop/{stream_id}", app.StreamDisableHandler()).Methods("PUT")
	app.Router.Handle("/streams/delete/{stream_id}", app.StreamDeleteHandler()).Methods("PUT")
	app.Router.Handle("/streams/sync/{stream_id}", app.StreamSyncHandler()).Methods("GET")
	app.Router.Handle("/core/start", app.CoreStartHandler()).Methods("GET")
	app.Router.Handle("/core/frame", app.CoreFrameHandler()).Methods("PUT")
	app.Router.Handle("/core/checkpoint", app.CoreCheckpointHandler()).Methods("PUT")
	app.Router.Handle("/core/stop", app.CoreStopHandler()).Methods("PUT")
	app.Router.Handle("/core/heartbeat", app.CoreHeartbeatHandler()).Methods("POST")
	app.server = NewServer(config.InternalHost, app.Router)

	fmt.Println("finished setting up router")

	if len(config.SSL) > 0 {
		app.server.TLS(config.SSL["Cert"], config.SSL["Key"])
		// app.server.CA(config.SSL["CA"])
	}
	app.statsWG.Add(1)
	return &app
}

func (app *Application) StreamsCursor() *mgo.Collection {
	return app.Mongo.DB("streams").C(app.Config.Name)
}

type AppHandler func(http.ResponseWriter, *http.Request) error

// When a handler returns an non-nil error, this method sets the status code to 400.
func (fn AppHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	code := 200
	if err := fn(w, r); err != nil {
		http.Error(w, err.Error(), 400)
		code = 400
	}
	log.Printf("%s %s %s %d", r.RemoteAddr, r.Method, r.URL, code)
}

// Look up the User using the Authorization header
func (app *Application) CurrentUser(r *http.Request) (user string, err error) {
	token := r.Header.Get("Authorization")
	cursor := app.Mongo.DB("users").C("all")
	result := make(map[string]interface{})
	if err = cursor.Find(bson.M{"token": token}).One(&result); err != nil {
		return
	}
	user = result["_id"].(string)
	return
}

// Returns True if user is a manager.
func (app *Application) IsManager(user string) bool {
	cursor := app.Mongo.DB("users").C("managers")
	result := make(map[string]interface{})
	if err := cursor.Find(bson.M{"_id": user}).One(&result); err != nil {
		return false
	} else {
		return true
	}
}

func (app *Application) CurrentManager(r *http.Request) (user string, err error) {
	user, err = app.CurrentUser(r)
	if err != nil {
		return "", errors.New("Unable to find user.")
	}
	isManager := app.IsManager(user)
	if isManager == false {
		return "", errors.New("Not a manager.")
	}
	return user, nil
}

// Return a path indicating where stream files should be stored
func (app *Application) StreamDir(stream_id string) string {
	return filepath.Join(app.Config.Name+"_data", "streams", stream_id)
}

// Starts the server. Listens and serves asynchronously. Also sets up necessary
// signal handlers for graceful termination. This blocks until a signal is sent
func (app *Application) Run() {
	log.Printf("Starting up server (pid: %d) on %s", os.Getpid(), app.Config.InternalHost)
	// log.Printf("Internal host: %s, external host: %s", app.Config.InternalHost, app.Config.ExternalHost)
	app.RegisterSCV()
	app.LoadStreams()
	go func() {
		log.Println("Success! Now serving requests...")
		err := app.server.ListenAndServe()
		if err != nil {
			log.Println("ListenAndServe: ", err)
		}
	}()
	go app.RecordDeferredDocs()
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill, syscall.SIGTERM)
	<-c
	app.Shutdown()
}

func (app *Application) Shutdown() {
	log.Printf("Shutting down gracefully...")
	app.server.Close()
	close(app.finish)
	app.statsWG.Wait()
	app.Mongo.Close()
}

func (app *Application) AliveHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) (err error) {
		return nil
	}
}

/*
.. http:post:: /streams/activate
    Activate and return the highest priority stream of a target by
    popping the head of the priority queue.
    .. note:: This request can only be made by CCs.
    **Example request**
    .. sourcecode:: javascript
        {
            "target_id": "some_uuid4",
            "engine": "engine_name",
            "user": "jesse_v" // optional
        }
    **Example reply**
    .. sourcecode:: javascript
        {
            "token": "uuid token"
        }
    :status 200: OK
    :status 400: Bad request
*/
func (app *Application) StreamActivateHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) (err error) {
		if r.Header.Get("Authorization") != app.Config.Password {
			return errors.New("Unauthorized")
		}
		type Message struct {
			TargetId string `json:"target_id"`
			Engine   string `json:"engine"`
			User     string `json:"user"`
		}
		msg := Message{}
		decoder := json.NewDecoder(r.Body)
		err = decoder.Decode(&msg)
		if err != nil {
			return errors.New("Bad request: " + err.Error())
		}
		fn := func(s *Stream) error {
			err := os.RemoveAll(filepath.Join(app.StreamDir(s.StreamId), "buffer_files"))
			return err
		}
		token, _, err := app.Manager.ActivateStream(msg.TargetId, msg.User, msg.Engine, fn)
		if err != nil {
			return errors.New("Unable to activate stream: " + err.Error())
		}
		type Reply struct {
			token string
		}
		data, _ := json.Marshal(map[string]string{"token": token})
		w.Write(data)
		return
	}
}

func splitExt(path string) (root string, ext string) {
	ext = filepath.Ext(path)
	root = path[0 : len(path)-len(ext)]
	return
}

func maxCheckpoint(path string) (int, error) {
	checkpointDirs, e := ioutil.ReadDir(path)
	if e != nil {
		return 0, errors.New("Cannot read frames directory")
	}
	// find the folder containing the last checkpoint
	lastCheckpoint := 0
	for _, fileProp := range checkpointDirs {
		count, _ := strconv.Atoi(fileProp.Name())
		if count > lastCheckpoint {
			lastCheckpoint = count
		}
	}
	return lastCheckpoint, nil
}

/*
.. http:get:: /streams/download/:stream_id/:filename
	Download file ``filename`` from ``stream_id``. ``filename`` can be
	either a file in ``files`` or a frame file posted by the core.
	If it is a frame file, then the frames are concatenated on the fly
	before returning.
	.. note:: Even if ``filename`` is not found, this handler will
	    return an empty file with the status code set to 200. This is
	    because we cannot distinguish between a frame file that has not
	    been received from that of a non-existent file.
	:reqheader Authorization: manager authorization token
	:resheader Content-Type: application/octet-stream
	:resheader Content-Disposition: attachment; filename=filename
	:resheader Content-Length: size of file
	:status 200: OK
	:status 400: Bad request

*/
func (app *Application) StreamDownloadHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) (err error) {
		streamId := mux.Vars(r)["stream_id"]
		file := mux.Vars(r)["file"]
		absStreamDir, _ := filepath.Abs(filepath.Join(app.StreamDir(streamId)))
		requestedFile, _ := filepath.Abs(filepath.Join(app.StreamDir(streamId), file))
		if len(requestedFile) < len(absStreamDir) {
			return errors.New("Invalid file path")
		}
		if requestedFile[0:len(absStreamDir)] != absStreamDir {
			return errors.New("Invalid file path.")
		}
		user, err := app.CurrentUser(r)
		if err != nil {
			return errors.New("Unable to find user.")
		}
		return app.Manager.ReadStream(streamId, func(stream *Stream) error {
			if stream.Owner != user {
				return errors.New("You do not own this stream.")
			}
			binary, e := ioutil.ReadFile(requestedFile)
			if e != nil {
				return errors.New("Unable to read file.")
			}
			w.Write(binary)
			return nil
		})
	}
}

// Return the number of partitions in a stream.
func (app *Application) ListPartitions(streamId string) ([]int, error) {
	res := make([]int, 0)
	files, err := ioutil.ReadDir(app.StreamDir(streamId))
	if err != nil {
		return nil, errors.New("FATAL StreamSyncHandler(), can't read streamDir")
	}
	for _, fileInfo := range files {
		num, err2 := strconv.Atoi(fileInfo.Name())
		if err2 == nil && num > 0 {
			res = append(res, num)
		}
	}
	sort.Ints(res)
	return res, nil
}

/*
.. http:get:: /streams/sync/:stream_id
    Retrieve the information needed to sync data back in an efficient
    manner. This method does not invoke os.walk() or anything that
    requires invoking stat on a large number of files.
    If the partition is comprised of the list [5, 12, 38], then the
    stream is divided into the partition (0, 5](5, 12](12, 38], where
    (a,b] denote the open and closed ends.
    :reqheader Authorization: Manager token
    **Example reply**:
    .. sourcecode:: javascript
        {
            'partitions': [5, 12, 38],
            'frame_files': ['frames.xtc', 'log.txt'],
            'checkpoint_files': ['state.xml.gz.b64']
            'seed_files': ['state.xml.gz.b64', 'system.xml.gz.b64',
                           'integrator.xml.gz.b64']
        }
    .. note:: If 'partitions' is not an empty list, then 'frame_files'
        and 'checkpoint_files' are present.
*/
func (app *Application) StreamSyncHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) error {
		streamId := mux.Vars(r)["stream_id"]
		user, auth_err := app.CurrentManager(r)
		if auth_err != nil {
			return auth_err
		}

		result := make(map[string]interface{})

		listSeeds := func() []string {
			seedDir := filepath.Join(app.StreamDir(streamId), "files")
			files, err := ioutil.ReadDir(seedDir)
			if err != nil {
				panic("FATAL StreamSyncHandler(), can't read seedDir" + seedDir)
			}
			res := make([]string, 0)
			for _, fileInfo := range files {
				res = append(res, fileInfo.Name())
			}
			return res
		}

		listFramesAndCheckpoints := func(min_partition int) ([]string, []string) {

			frames := make([]string, 0)
			checkpoints := make([]string, 0)

			frameDir := filepath.Join(app.StreamDir(streamId), strconv.Itoa(min_partition), "0")

			frameFiles, err := ioutil.ReadDir(frameDir)
			if err != nil {
				panic("FATAL StreamSyncHandler(), can't read frameDir: " + frameDir)
			}
			for _, fileInfo := range frameFiles {
				if fileInfo.Name() != "checkpoint_files" {
					frames = append(frames, fileInfo.Name())
				}
			}
			checkpointDir := filepath.Join(frameDir, "checkpoint_files")
			checkpointFiles, err := ioutil.ReadDir(checkpointDir)
			if err != nil {
				panic("FATAL StreamSyncHandler(), can't read checkpointDir: " + checkpointDir)
			}
			for _, fileInfo := range checkpointFiles {
				if fileInfo.Name() != "checkpoint_files" {
					checkpoints = append(checkpoints, fileInfo.Name())
				}
			}
			return frames, checkpoints
		}

		e := app.Manager.ReadStream(streamId, func(stream *Stream) error {
			if stream.Owner != user {
				return errors.New("You do not own this stream.")
			}
			partitions, err := app.ListPartitions(streamId)
			if err != nil {
				return err
			}
			result["partitions"] = partitions
			result["seed_files"] = listSeeds()
			if len(partitions) > 0 {
				result["frame_files"], result["checkpoint_files"] = listFramesAndCheckpoints(partitions[0])
			}
			return nil
		})
		if e != nil {
			return e
		}
		data, e := json.Marshal(result)
		if e != nil {
			return e
		}
		w.Write(data)
		return nil
	}
}

/*
 .. http:put:: /streams/enable/:stream_id
    Enable a stream, making it eligible to be assigned.
    :reqheader Authorization: Manager's authorization token
    **Example request**:
    .. sourcecode:: javascript
        {
            // empty
        }
    :status 200: OK
    :status 400: Bad request
*/
func (app *Application) StreamEnableHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) error {
		user, auth_err := app.CurrentManager(r)
		if auth_err != nil {
			return auth_err
		}
		streamId := mux.Vars(r)["stream_id"]
		return app.Manager.EnableStream(streamId, user)
	}
}

/*
 .. http:put:: /streams/disable/:stream_id
    Disable a stream, making it ineligible to be assigned.
    :reqheader Authorization: Manager's authorization token
    **Example request**:
    .. sourcecode:: javascript
        {
            // empty
        }
    :status 200: OK
    :status 400: Bad request
*/
func (app *Application) StreamDisableHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) error {
		user, auth_err := app.CurrentManager(r)
		if auth_err != nil {
			return auth_err
		}
		streamId := mux.Vars(r)["stream_id"]
		return app.Manager.DisableStream(streamId, user)
	}
}

/*
 .. http:put:: /streams/delete/:stream_id
    Delete a stream permanently.
    :reqheader Authorization: Manager's authorization token
    **Example request**:
    .. sourcecode:: javascript
        {
            // empty
        }
    .. note:: When all streams belonging to a target is removed, the
        target and shard information is cleaned up automatically.
    :status 200: OK
    :status 400: Bad request
*/
func (app *Application) StreamDeleteHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) error {
		streamId := mux.Vars(r)["stream_id"]
		user, auth_err := app.CurrentManager(r)
		if auth_err != nil {
			return auth_err
		}
		err := app.Manager.RemoveStream(streamId, user)
		if err != nil {
			return err
		}
		fn1 := func() error {
			return app.StreamsCursor().RemoveId(streamId)
		}
		app.statsMutex.Lock()
		app.stats.PushBack(fn1)
		app.statsMutex.Unlock()
		return nil
	}
}

/*
.. http:post:: /streams
    Add a new stream to this SCV.
    **Example request**
    .. sourcecode:: javascript
        {
            "target_id": "target_id",
            "files": {"system.xml.gz.b64": "file1.b64",
                "integrator.xml.gz.b64": "file2.b64",
                "state.xml.gz.b64": "file3.b64"
            }
            "tags": {
                "pdb.gz.b64": "file4.b64",
            } // optional
        }
    .. note:: Binary files must be base64 encoded.
    .. note:: tags are files that are not used by the core.
    **Example reply**
    .. sourcecode:: javascript
        {
            "stream_id" : "715c592f-8487-46ac-a4b6-838e3b5c2543:hello"
        }
    :status 200: OK
    :status 400: Bad request
*/
func (app *Application) StreamsHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) (err error) {
		user, auth_err := app.CurrentManager(r)
		if auth_err != nil {
			return auth_err
		}
		type Message struct {
			TargetId string            `json:"target_id"`
			Files    map[string]string `json:"files"`
			Tags     map[string]string `json:"tags,omitempty"`
		}
		msg := Message{}
		decoder := json.NewDecoder(r.Body)
		err = decoder.Decode(&msg)
		if err != nil {
			return errors.New("Bad request: " + err.Error())
		}
		streamId := RandSeq(36) + ":" + app.Config.Name
		// Add files to disk
		stream := NewStream(streamId, msg.TargetId, user, 0, 0, int(time.Now().Unix()))
		todo := map[string]map[string]string{"files": msg.Files, "tags": msg.Tags}
		for Directory, Content := range todo {
			for filename, fileb64 := range Content {
				files_dir := filepath.Join(app.StreamDir(streamId), Directory)
				os.MkdirAll(files_dir, 0776)
				err = ioutil.WriteFile(filepath.Join(files_dir, filename), []byte(fileb64), 0776)
				if err != nil {
					return err
				}
			}
		}
		cursor := app.StreamsCursor()
		err = cursor.Insert(stream)
		if err != nil {
			// clean up
			os.RemoveAll(app.StreamDir(streamId))
			return errors.New("Unable insert stream into DB")
		}
		// Insert stream into Manager after ensuring state is correct.
		e := app.Manager.AddStream(stream, msg.TargetId, true)
		if e != nil {
			return e
		}
		data, err := json.Marshal(map[string]string{"stream_id": streamId})
		if e != nil {
			return e
		}
		w.Write(data)
		return
	}
}

func (app *Application) StreamInfoHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) (err error) {
		//cursor := app.StreamsCursor()
		//msg := mongoStream{}
		streamId := mux.Vars(r)["stream_id"]
		var result []byte
		var isActive bool
		e := app.Manager.ReadStream(streamId, func(stream *Stream) error {
			if stream.activeStream != nil {
				isActive = true
			} else {
				isActive = false
			}
			result, err = json.Marshal(stream)
			return err
		})
		if e != nil {
			return e
		}
		tmp := make(map[string]interface{})
		json.Unmarshal(result, &tmp)
		tmp["active"] = isActive
		result_final, _ := json.Marshal(tmp)
		w.Write(result_final)
		return nil
	}
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (app *Application) ActiveStreamsHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) error {
		data, e := json.Marshal(app.Manager.GetActiveStreams())
		if e != nil {
			return e
		}
		w.Write(data)
		return nil
	}
}

/*
 ..  http:put:: /core/frame
    Append a frame to the stream's buffer.
    If the core posts to this method, then the WS assumes that the
    frame is valid. The data received is stored in a buffer until a
    checkpoint is received. It is assumed that files given here are
    binary appendable. Files ending in .b64 or .gz are decoded
    automatically.
    :reqheader Content-MD5: MD5 Sum of the body
    :reqheader Authorization: core Authorization token
    **Example request**
    .. sourcecode:: javascript
        {
            "files" : {
                "frames.xtc.b64": "file.b64",
                "log.txt.gz.b64": "file.gz.b64"
            },
            "frames": 25,  // optional, number of frames in the files
        }
    :status 200: OK
    :status 400: Bad request
*/
func (app *Application) CoreFrameHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) (err error) {
		token := r.Header.Get("Authorization")
		md5String := r.Header.Get("Content-MD5")
		body, _ := ioutil.ReadAll(r.Body)
		h := md5.New()
		io.WriteString(h, string(body))
		if md5String != hex.EncodeToString(h.Sum(nil)) {
			return errors.New("MD5 mismatch")
		}
		return app.Manager.ModifyActiveStream(token, func(stream *Stream) error {
			type Message struct {
				Files  map[string]string `json:"files"`
				Frames int               `json:"frames"`
			}
			msg := Message{Frames: 1}
			decoder := json.NewDecoder(bytes.NewReader(body))
			err := decoder.Decode(&msg)
			if err != nil {
				return errors.New("Could not decode JSON")
			}
			if md5String == stream.activeStream.frameHash {
				return errors.New("POSTed same frame twice")
			}
			stream.activeStream.frameHash = md5String
			for filename, filestring := range msg.Files {
				root, ext := splitExt(filename)
				filebin := []byte(filestring)
				if ext == ".b64" {
					filename = root
					reader := base64.NewDecoder(base64.StdEncoding, bytes.NewReader(filebin))
					filecopy, err := ioutil.ReadAll(reader)
					if err != nil {
						return err
					}
					filebin = filecopy
					root, ext := splitExt(filename)
					if ext == ".gz" {
						filename = root
						reader, err := gzip.NewReader(bytes.NewReader(filebin))
						defer reader.Close()
						if err != nil {
							return err
						}
						filecopy, err := ioutil.ReadAll(reader)
						if err != nil {
							return err
						}
						filebin = filecopy
					}
				}
				dir := filepath.Join(app.StreamDir(stream.StreamId), "buffer_files")
				os.MkdirAll(dir, 0776)
				filename = filepath.Join(dir, filename)
				file, err := os.OpenFile(filename, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0776)
				defer file.Close()
				if err != nil {
					return err
				}
				_, err = file.Write(filebin)
				if err != nil {
					return err
				}
			}
			stream.activeStream.bufferFrames += 1
			return nil
		})
	}
}

/*
.. http:put:: /core/checkpoint
    Add a checkpoint and flushes buffered files into a state deemed
    safe. It is assumed that the checkpoint corresponds to the last
    frame of the buffered frames.
    :reqheader Content-MD5: MD5 Sum of the body
    :reqheader Authorization: core Authorization token
    **Example Request**
    .. sourcecode:: javascript
        {
            "files": {
                "state.xml.gz.b64" : "state.xml.gz.b64"
            },
            "frames": 239.98, # number of frames since last checkpoint
        }
    .. note:: filenames must be almost be present in stream_files
    .. note:: If ``frames`` is not provided, the backend uses
        buffer frames an approximation
    :status 200: OK
    :status 400: Bad request
*/
func (app *Application) CoreCheckpointHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) (err error) {
		token := r.Header.Get("Authorization")
		md5String := r.Header.Get("Content-MD5")
		body, _ := ioutil.ReadAll(r.Body)
		h := md5.New()
		io.WriteString(h, string(body))
		if md5String != hex.EncodeToString(h.Sum(nil)) {
			return errors.New("MD5 mismatch")
		}
		return app.Manager.ModifyActiveStream(token, func(stream *Stream) error {
			streamDir := app.StreamDir(stream.StreamId)
			bufferDir := filepath.Join(streamDir, "buffer_files")
			checkpointDir := filepath.Join(bufferDir, "checkpoint_files")
			os.MkdirAll(checkpointDir, 0776)
			type Message struct {
				Files  map[string]string `json:"files"`
				Frames float64           `json:"frames"`
			}
			msg := Message{}
			decoder := json.NewDecoder(bytes.NewReader(body))
			err := decoder.Decode(&msg)
			if err != nil {
				return errors.New("Could not decode JSON")
			}
			for filename, filestring := range msg.Files {
				fileDir := filepath.Join(checkpointDir, filename)
				fileBin := []byte(filestring)
				ioutil.WriteFile(fileDir, fileBin, 0776)
			}
			bufferFrames := stream.activeStream.bufferFrames
			sumFrames := stream.Frames + bufferFrames
			partition := filepath.Join(streamDir, strconv.Itoa(sumFrames))
			os.MkdirAll(partition, 0766)
			var renameDir string

			if bufferFrames == 0 {
				exist, _ := pathExists(partition)
				if exist {
					lastCheckpoint, _ := maxCheckpoint(partition)
					renameDir = filepath.Join(partition, strconv.Itoa(lastCheckpoint+1))
				} else {
					renameDir = filepath.Join(partition, "1")
				}
			} else {
				renameDir = filepath.Join(partition, "0")
			}
			os.Rename(bufferDir, renameDir)
			stream.Frames = sumFrames
			stream.activeStream.donorFrames += msg.Frames
			stream.activeStream.bufferFrames = 0
			// TODO: update frame count in MongoDB (do we want to?)
			// This stream is mutex'd
			return nil
		})
	}
}

/*
.. http:get:: /core/start
    Get files needed for the core to start an activated stream.
    :reqheader Authorization: core Authorization token
    :resheader Content-MD5: MD5 hexdigest of the body
    **Example reply**
    .. sourcecode:: javascript
        {
            "stream_id": "uuid4",
            "target_id": "uuid4",
            "files": {
                "state.xml.gz.b64": "content.b64",
                "integrator.xml.gz.b64": "content.b64",
                "system.xml.gz.b64": "content.b64"
            }
            "options": {
                "steps_per_frame": 50000,
                "title": "Dihydrofolate Reductase", // used by some
                "description": "This protein is the canonical benchmark
                    protein used by the MD community."
                "category": "Benchmark"
            }
        }
    :status 200: OK
    :status 400: Bad request
*/
func (app *Application) CoreStartHandler() AppHandler {

	// We need to be extremely careful about checkpoints and frames, as
	// it is important we avoid writing duplicate frames on the first
	// step for the core. We use the follow scheme:
	//
	//               (0,10]                      (10,20]
	//             frameset_10                 frameset_20
	//      -------------------------------------------------------------
	//      |c        core 1      |c|              core 2         |c|
	//      ----                  --|--                           --|--
	// frame x |1 2 3 4 5 6 7 8 9 10| |11 12 13 14 15 16 17 18 19 20| |21
	//         ---------------------| ------------------------------- ---
	//
	// In other words, the core does not write frames for the zeroth frame.

	return func(w http.ResponseWriter, r *http.Request) (err error) {
		token := r.Header.Get("Authorization")
		type Reply struct {
			StreamId string            `json:"stream_id"`
			TargetId string            `json:"target_id"`
			Files    map[string]string `json:"files"`
			Options  interface{}       `json:"options"`
		}
		rep := Reply{
			Files:   make(map[string]string),
			Options: make(map[string]interface{}),
		}
		e := app.Manager.ModifyActiveStream(token, func(stream *Stream) error {
			rep.StreamId = stream.StreamId
			rep.TargetId = stream.TargetId
			// Load stream's options from Mongo
			cursor := app.Mongo.DB("data").C("targets")
			mgoRes := make(map[string]interface{})
			if err = cursor.Find(bson.M{"_id": stream.TargetId}).One(&mgoRes); err != nil {
				return errors.New("Cannot load target's options")
			}
			rep.Options = mgoRes["options"]
			// Load the streams' files
			if stream.Frames > 0 {
				frameDir := filepath.Join(app.StreamDir(rep.StreamId), strconv.Itoa(stream.Frames))
				lastCheckpoint, _ := maxCheckpoint(frameDir)
				checkpointDir := filepath.Join(frameDir, strconv.Itoa(lastCheckpoint), "checkpoint_files")
				checkpointFiles, e := ioutil.ReadDir(checkpointDir)
				if e != nil {
					return errors.New("Cannot load checkpoint directory")
				}
				for _, fileProp := range checkpointFiles {
					binary, e := ioutil.ReadFile(filepath.Join(checkpointDir, fileProp.Name()))
					if e != nil {
						return errors.New("Cannot read checkpoint file")
					}
					rep.Files[fileProp.Name()] = string(binary)
				}
			}
			seedDir := filepath.Join(app.StreamDir(rep.StreamId), "files")
			seedFiles, e := ioutil.ReadDir(seedDir)
			if e != nil {
				return errors.New("Cannot read seed directory")
			}
			for _, fileProp := range seedFiles {
				_, ok := rep.Files[fileProp.Name()]
				if ok == false {
					binary, e := ioutil.ReadFile(filepath.Join(seedDir, fileProp.Name()))
					if e != nil {
						return errors.New("Cannot read seed files")
					}
					rep.Files[fileProp.Name()] = string(binary)
				}
			}
			return nil
		})
		if e != nil {
			return e
		}
		data, e := json.Marshal(rep)
		if e != nil {
			return e
		}
		w.Write(data)
		return
	}
}

/*
..  http:put:: /core/stop
    Stop the stream and deactivate.
    :reqheader Authorization: core Authorization token
    **Example Request**
    .. sourcecode:: javascript
        {
            "error": "message_b64",  // optional
        }
    .. note:: ``error`` must be b64 encoded.
    :status 200: OK
    :status 400: Bad request
*/
func (app *Application) CoreStopHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) (err error) {
		token := r.Header.Get("Authorization")
		type Message struct {
			Error string `json:"error"`
		}
		msg := Message{}
		if r.Body != nil {
			decoder := json.NewDecoder(r.Body)
			err = decoder.Decode(&msg)
			if err != nil {
				return
			}
		}
		error_count := 0
		if msg.Error != "" {
			error_count += 1
		}
		return app.Manager.DeactivateStream(token, error_count)
	}
}

/*
.. http:post:: /core/heartbeat
    Cores POST to this handler to notify the WS that it is still
    alive.
    :reqheader Authorization: core Authorization token
    :status 200: OK
    :status 400: Bad request
*/
func (app *Application) CoreHeartbeatHandler() AppHandler {
	return func(w http.ResponseWriter, r *http.Request) (err error) {
		token := r.Header.Get("Authorization")
		return app.Manager.ResetActiveStream(token)
	}
}
