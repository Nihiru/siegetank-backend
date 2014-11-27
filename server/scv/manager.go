package scv

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"../util"
)

var _ = fmt.Printf

// A mechanism for allowing dependency injection. These methods will usually close other external app variables like DBs, file/io, etc.
type Injector interface {
	DeactivateStreamService(*Stream) error
}

// The mutex in Manager makes guarantees about the state of the system:
// 1. If the mutex (read or write) can be acquired, then there is no other concurrent operation that could affect stream creation, deletion, activation, or deactivation, target creation and deletion.
// 2. If you acquire a read lock, you still need to lock individual targets and streams when modifying them (eg. when posting frames).
// 3. Note that the stream or target may already have been removed by another operation, so it is important that you check the return value of anything you retrieve from the maps for existence. For example, it is possible that one goroutine is trying to deactivate the stream, while another goroutine is trying to post a frame. Both goroutines may be trying to acquire the lock at the same time. If the write goroutine acquires it first, then this means the read goroutine must verify the existence of the active stream through the token.
// 4. A target exists in the target map if and only if one or more of its streams exists in the streams map.
type Manager struct {
	sync.RWMutex
	targets        map[string]*Target // map of targetId to Target
	streams        map[string]*Stream // map of streamId to Stream
	injector       Injector
	expirationTime int // how long to wait on each stream if no heartbeat (in minutes)
}

func NewManager(inj Injector) *Manager {
	m := Manager{
		targets:        make(map[string]*Target),
		streams:        make(map[string]*Stream),
		injector:       inj,
		expirationTime: 1200,
	}
	return &m
}

func createToken(targetId string) string {
	return targetId + ":" + util.RandSeq(36)
}

func parseToken(token string) string {
	result := strings.Split(token, ":")
	if len(result) < 2 {
		return ""
	} else {
		return result[0]
	}
}

/*
Add a stream to the manager. If the stream exists, then nothing happens. Otherwise, if the target does not exist, a target
is created for this stream. It is assumed that the respective persistent structures (dbs, files) for this
stream has already been created and ready to go. It is assumed that while AddStream is called, no other goroutine is manipulating
this particular stream pointer.
*/
func (m *Manager) AddStream(stream *Stream, targetId string) error {
	m.Lock()
	defer m.Unlock()
	_, ok := m.streams[stream.StreamId]
	if ok == true {
		return errors.New("stream " + stream.StreamId + " already exists")
	}
	m.streams[stream.StreamId] = stream
	_, ok = m.targets[targetId]
	if ok == false {
		m.targets[targetId] = NewTarget()
	}
	t := m.targets[targetId]
	t.Lock()
	defer t.Unlock()
	t.inactiveStreams.Add(stream)
	return nil
}

/*
Remove a stream from the manager. The stream is immediately removed. It is up to the calling function to specify clean-up behavior.
We need to lock the stream here because other functions may be using it.
*/
func (m *Manager) RemoveStream(streamId string) error {
	m.Lock()
	defer m.Unlock()
	stream, ok := m.streams[streamId]
	if ok == false {
		return errors.New("stream " + streamId + " does not exist")
	}
	t := m.targets[stream.TargetId]
	t.Lock()
	defer t.Unlock()
	stream.Lock()
	defer stream.Unlock()
	delete(m.streams, streamId)
	if stream.activeStream != nil {
		t.deactivateStreamImpl(stream)
	}
	t.inactiveStreams.Remove(stream)
	if len(t.activeStreams) == 0 && t.inactiveStreams.Len() == 0 {
		delete(m.targets, stream.TargetId)
	}
	return nil
}

func (m *Manager) ReadStream(streamId string, fn func(*Stream) error) error {
	m.RLock()
	stream, ok := m.streams[streamId]
	if ok == false {
		m.RUnlock()
		return errors.New("stream " + streamId + " does not exist")
	}
	t := m.targets[stream.TargetId]
	t.RLock()
	stream.RLock()
	t.RUnlock()
	m.RUnlock()
	defer stream.RUnlock()
	return fn(stream)
}

func (m *Manager) ModifyStream(streamId string, fn func(*Stream) error) error {
	m.RLock()
	stream, ok := m.streams[streamId]
	if ok == false {
		m.RUnlock()
		return errors.New("stream " + streamId + " does not exist")
	}
	t := m.targets[stream.TargetId]
	t.RLock()
	stream.Lock() // Acquire a write lock
	t.RUnlock()
	m.RUnlock()
	defer stream.Unlock()
	return fn(stream)
}

func (m *Manager) ModifyActiveStream(token string, fn func(*Stream) error) error {
	m.RLock()
	targetId := parseToken(token)
	if targetId == "" {
		m.RUnlock()
		return errors.New("invalid token: " + token)
	}
	t, ok := m.targets[targetId]
	if ok == false {
		m.RUnlock()
		return errors.New("invalid parsed target: " + targetId)
	}
	t.RLock()
	stream, ok := t.tokens[token]
	if ok == false {
		t.RUnlock()
		m.RUnlock()
		return errors.New("invalid token: " + token)
	}
	stream.Lock()
	t.RUnlock()
	m.RUnlock()
	defer stream.Unlock()
	return fn(stream)
}

func (m *Manager) ActivateStream(targetId, user, engine string) (token string, streamId string, err error) {
	m.RLock()
	defer m.RUnlock()
	t, ok := m.targets[targetId]
	if ok == false {
		err = errors.New("Target does not exist")
		return
	}
	t.Lock()
	defer t.Unlock()
	iterator := t.inactiveStreams.Iterator()
	ok = iterator.Next()
	if ok == false {
		err = errors.New("Target does not have streams")
		return
	}
	token = createToken(targetId)
	stream := iterator.Key().(*Stream)
	streamId = stream.StreamId
	stream.Lock()
	defer stream.Unlock()
	t.inactiveStreams.Remove(stream)
	stream.activeStream = NewActiveStream(user, token, engine)
	t.tokens[token] = stream
	t.timers[stream.StreamId] = time.AfterFunc(time.Second*time.Duration(m.expirationTime), func() {
		m.DeactivateStream(token)
	})
	t.activeStreams[stream] = struct{}{}
	return
}

// // This returns by copy
// func (m *Manager) ActiveStreams(targetId string) (result map[ActiveStream]struct{}, err error) {
// 	m.RLock()
// 	defer m.RUnlock()
// 	t, ok := m.targets[targetId]
// 	if ok == false {
// 		err = errors.New("Target does not exist")
// 		return
// 	}
// 	t.RLock()
// 	defer t.RUnlock()
// 	result = make(map[ActiveStream]struct{})
// 	for token := range t.tokens {
// 		result[*t.tokens[token].activeStream] = struct{}{}
// 	}
// 	return
// }

// Assumes that locks are in place.
func (m *Manager) deactivateStreamImpl(s *Stream, t *Target) {
	delete(t.tokens, s.activeStream.authToken)
	delete(t.timers, s.StreamId)
	delete(t.activeStreams, s)
	s.activeStream = nil
	t.inactiveStreams.Add(s)
}

// func (m *Manager) ModifyActiveStream(token string, fn func(*Stream) error) error {
// 	m.RLock()
// 	targetId := parseToken(token)
// 	if targetId == "" {
// 		m.RUnlock()
// 		return errors.New("invalid token: " + token)
// 	}
// 	t, ok := m.targets[targetId]
// 	if ok == false {
// 		m.RUnlock()
// 		return errors.New("invalid parsed target: " + targetId)
// 	}
// 	t.RLock()
// 	stream, ok := t.tokens[token]
// 	if ok == false {
// 		t.RUnlock()
// 		m.RUnlock()
// 		return errors.New("invalid token: " + token)
// 	}
// 	stream.Lock()
// 	t.RUnlock()
// 	m.RUnlock()
// 	defer stream.Unlock()
// 	return fn(stream)
// }

func (m *Manager) DeactivateStream(token string) error {
	m.RLock()
	targetId := parseToken(token)
	if targetId == "" {
		m.RUnlock()
		return errors.New("invalid token: " + token)
	}
	t, ok := m.targets[targetId]
	if ok == false {
		m.RUnlock()
		return errors.New("invalid parsed target: " + targetId)
	}
	t.Lock()
	stream, ok := t.tokens[token]
	if ok == false {
		t.Unlock()
		m.RUnlock()
		return errors.New("invalid token: " + token)
	}
	stream.Lock()
	defer stream.Unlock()
	t.deactivateStreamImpl(stream)
	t.Unlock()
	m.RUnlock()
	return m.injector.DeactivateStreamService(stream)
}
