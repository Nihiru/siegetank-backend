package scv

import (
	"sync"
	"time"
)

// Cached object persisted in Mongo
type Stream struct {
	sync.RWMutex `json:"-" bson:"-"`
	Owner        string `json:"-" bson:"-"`                 // constant (safe to read without mutex)
	StreamId     string `json:"-" bson:"_id"`               // constant
	TargetId     string `json:"target_id" bson:"target_id"` // constant
	Frames       int    `json:"frames" bson:"frames"`
	ErrorCount   int    `json:"error_count" bson:"error_count"`
	CreationDate int    `json:"creation_date" bson:"creation_date"`

	MongoStatus string `json:"status" bson:"status"` // this value is used only for persistence purposes.

	activeStream *ActiveStream
}

func NewStream(streamId, targetId, owner string,
	frames, errorCount, creationDate int) *Stream {
	stream := &Stream{
		StreamId:     streamId,
		TargetId:     targetId,
		Frames:       frames,
		ErrorCount:   errorCount,
		CreationDate: creationDate,
		Owner:        owner,
		MongoStatus:  "enabled", // by default is enabled because we can't
	}
	return stream
}

type ActiveStream struct {
	donorFrames  float64 // number of frames done by this donor (including partial frames)
	bufferFrames int     // number of frames stored in the buffer
	authToken    string  // token of the ActiveStream
	user         string  // donor id
	startTime    int     // time the stream was activated
	frameHash    string  // md5 hash of the last frame
	engine       string  // core engine type the stream is assigned to
	timer        *time.Timer
}

func NewActiveStream(user, token, engine string) *ActiveStream {
	as := &ActiveStream{
		user:      user,
		engine:    engine,
		authToken: token,
		startTime: int(time.Now().Unix()),
	}
	return as
}
