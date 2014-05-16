# Authors: Yutong Zhao <proteneer@gmail.com>
#
# Licensed under the Apache License, Version 2.0 (the "License"); you may
# not use this file except in compliance with the License. You may obtain
# a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
# WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
# License for the specific language governing permissions and limitations
# under the License.

from __future__ import print_function, absolute_import, division

import requests
import json
import base64
import hashlib
import functools
import time
from siegetank.util import is_domain, encode_files

auth_token = None
login_cc = None
scvs = dict()
last_scvs_refresh = 0


def login(cc, token):
    """ Login to a particular command center using your token. """
    url = 'https://'+cc+'/users/verify'
    headers = {'Authorization': token}
    reply = requests.get(url, verify=is_domain(cc), headers=headers)
    if reply.status_code != 200:
        print(reply.content)
    global auth_token
    auth_token = token
    global login_cc
    login_cc = cc
    refresh_scvs()

def require_login(method):
    """ Decorator for methods that require logging in. """
    @functools.wraps(method)
    def wrapper(*args, **kwargs):
        global auth_token
        if auth_token is None:
            raise ValueError('You are not logged in to a cc.')
        else:
            return method(*args, **kwargs)
    return wrapper


@require_login
def refresh_scvs():
    """ Update and return the status of the SCVs. This method is rate limited
        to once every 5 seconds. """
    global scvs
    global last_scvs_refresh
    global login_cc
    if time.time() - last_scvs_refresh > 5:
        url = 'https://'+login_cc+'/scvs/status'
        reply = requests.get(url, verify=is_domain(login_cc))
        if reply.status_code == 200:
            content = reply.json()
            for scv_name, scv_prop in content.items():
                # sets host and status fields
                scvs[scv_name] = scv_prop
            last_scvs_refresh = time.time()


class Base:
    def __init__(self, uri):
        self.uri = uri

    def _get(self, path, host=None, headers=None):
        if headers is None:
            headers = {}
        headers['Authorization'] = auth_token
        if host is None:
            host = self.uri
        url = 'https://'+host+path
        return requests.get(url, headers=headers, verify=is_domain(self.uri),
                            timeout=2)

    def _put(self, path, body=None, headers=None):
        if headers is None:
            headers = {}
        headers['Authorization'] = auth_token
        url = 'https://'+self.uri+path
        if body is None:
            body = '{}'
        return requests.put(url, headers=headers, data=body,
                            verify=is_domain(self.uri), timeout=2)

    def _post(self, path, body=None, headers=None):
        if headers is None:
            headers = {}
        headers['Authorization'] = auth_token
        url = 'https://'+self.uri+path
        if body is None:
            body = '{}'
        return requests.post(url, headers=headers, data=body,
                             verify=is_domain(self.uri), timeout=2)


class Stream(Base):
    """ A Stream is a single trajectory residing on a remote server. """
    @require_login
    def __init__(self, stream_id):
        """ Retrieve an existing stream object.

        :param stream_id: str, id of the stream.

        """
        self._id = stream_id
        self._frames = None
        self._status = None
        self._error_count = None
        self._active = None
        scv_name = stream_id.split(':')[1]
        refresh_scvs()
        global scvs
        uri = scvs[scv_name]['host']
        super(Stream, self).__init__(uri)

    def __repr__(self):
        frames = str(self.frames)
        return '<stream '+self.id+' s:'+self.status+' f:'+frames+'>'

    def start(self):
        """ Start this stream. """
        reply = self._put('/streams/start/'+self.id)
        if reply.status_code != 200:
            print(reply.text)
            raise Exception('Bad status code')
        self.reload_info()

    def stop(self):
        """ Stop this stream. """
        reply = self._put('/streams/stop/'+self.id)
        if reply.status_code != 200:
            print(reply.text)
            raise Exception('Bad status code')
        self.reload_info()

    def delete(self):
        """ Delete this stream from the SCV. You must take care to not
        use this stream object anymore afterwards.

        """
        reply = self._put('/streams/delete/'+self.id)
        if reply.status_code != 200:
            print(reply.text)
            raise Exception('Bad status code')
        self._id = None

    def download(self, filename):
        """ Download a file from the stream.

        :param filename: name of the file. eg. '2/frames.xtc'.

        """
        reply = self._get('/streams/download/'+self.id+'/'+filename)
        return reply.content

    def upload(self, filename, filedata):
        """ Upload a file on the stream. The stream must be in the STOPPED
        state and the file must already exist.

        :param filename: name of the file, eg. state.xml.gz.b64
        :param filedata: binary data (do not b64 encode).

        """
        md5 = hashlib.md5(filedata).hexdigest()
        headers = {'Content-MD5': md5}
        reply = self._put('/streams/upload/'+self.id+'/'+filename,
                          body=filedata, headers=headers)
        if reply.status_code != 200:
            print(reply.text)
            raise Exception('Bad status code')

    def reload_info(self):
        reply = self._get('/streams/info/'+self.id)
        content = json.loads(reply.text)
        self._frames = content['frames']
        self._status = content['status']
        self._error_count = content['error_count']
        self._active = content['active']

    @property
    def files(self):
        """ Return a list of available files for stream. """
        reply = self._get('/streams/files/'+self.id)
        return reply.json()['files']

    @property
    def id(self):
        return self._id

    @property
    def active(self):
        """ Returns True if the stream is worked on by a core. """
        if not self._active:
            self.reload_info()
        return self._active

    @property
    def frames(self):
        """ Return the number of frames completed so far. """
        if not self._frames:
            self.reload_info()
        return self._frames

    @property
    def status(self):
        """ Return the status of the stream. """
        if not self._status:
            self.reload_info()
        return self._status

    @property
    def error_count(self):
        """ Return the number of errors this stream has encountered. """
        if not self._error_count:
            self.reload_info()
        return self._error_count


class Target(Base):
    """ A Target is a collection of Streams residing on a remote server. """
    @require_login
    def __init__(self, target_id):
        """ Retrieve an existing target object.

        :param target_id: str, id of the target.

        """
        global login_cc
        self._id = target_id
        self._options = None
        self._creation_date = None
        self._shards = None
        self._engines = None
        self._streams = None
        self._weight = None
        self._stage = None
        super(Target, self).__init__(login_cc)

    def __repr__(self):
        return '<target '+self.id+'>'

    def attach_shard(self):
        raise Exception('Not implemented')

    def detach_shard(self):
        raise Exception('Not implemented')

    def delete(self):
        """ Delete this target from the backend """
        reply = self._put('/targets/delete/'+self.id)
        if reply.status_code != 200:
            print(reply.text)
            raise Exception('Bad status code')
        self._id = None

    def update(self, options=None, engines=None, weight=None, stage=None):
        """ Update the target. This method cannot delete properties, only add
        or modify new properties.

        """
        message = dict()
        if options:
            message['options'] = options
        if engines:
            message['engines'] = engines
        if weight:
            message['weight'] = weight
        if stage:
            message['stage'] = stage
        message = json.dumps(message)
        reply = self._put('/targets/update/'+self.id, body=message)
        if reply.status_code != 200:
            raise Exception('could not update target. Reason:'+reply.content)
        self.reload_info()

    def add_stream(self, files, scv):
        """ Add a stream to the target belonging to a particular scv.

        :param files: dict, filenames and binaries matching the core's
            requirements.
        :param scv: str, which particular SCV to add the stream to.

        """
        assert isinstance(files, dict)
        body = {
            "target_id": self.id,
            "files": encode_files(files),
        }
        if scv:
            global scvs
            global auth_token
            refresh_scvs()
            url = 'https://'+scvs[scv]['host']+'/streams'
            headers = {'Authorization': auth_token}
            reply = requests.post(url, headers=headers, data=json.dumps(body),
                                  verify=is_domain(self.uri))
        else:
            reply = self._post('/streams', json.dumps(body))
        if reply.status_code != 200:
            print(reply.text)
            raise Exception('Bad status code')
        else:
            return json.loads(reply.text)['stream_id']

    def reload_streams(self):
        """ Reload the target's set of streams """
        self._streams = set()
        global scvs
        for scv in self.shards:
            host = scvs[scv]['host']
            reply = self._get('/targets/streams/'+self.id, host=host)
            if reply.status_code != 200:
                print(reply.status_code, reply.content)
                raise Exception('Failed to load streams from SCV: '+scv)
            for stream_id in reply.json()['streams']:
                self._streams.add(Stream(stream_id))

    def reload_info(self):
        """ Reload the target's information """
        reply = self._get('/targets/info/'+self.id)
        if reply.status_code != 200:
            raise Exception('Failed to load target info')
        info = json.loads(reply.text)
        self._options = info['options']
        self._creation_date = info['creation_date']
        self._shards = info['shards']
        self._engines = info['engines']
        self._weight = info['weight']
        self._stage = info['stage']

    @property
    def id(self):
        """ Get the target id """
        return self._id

    @property
    def streams(self):
        """ Get the set of streams in this target """
        self.reload_streams()
        return self._streams

    @property
    def options(self):
        """ Get the options for this target. """
        if not self._options:
            self.reload_info()
        return self._options

    @property
    def creation_date(self):
        """ Get the date the target was created. """
        if not self._creation_date:
            self.reload_info()
        return self._creation_date

    @property
    def shards(self):
        """ Return a list of SCVs that the streams are sharded across. """
        if not self._shards:
            self.reload_info()
        return self._shards

    @property
    def engines(self):
        """ Get the list of engines being used. """
        if not self._engines:
            self.reload_info()
        return self._engines

    @property
    def weight(self):
        return self._weight

    @property
    def stage(self):
        return self._stage


@require_login
def add_target(options, engines, weight=1, stage='private'):
    """ Add a target.

    :param options: dict, describing target's options.
    :param engine: list, eg. ["openmm_60_opencl", "openmm_50_cuda"]
    :param stage: str, stage of the target, allowed values are 'disabled',
        'private', 'public'
    :param weight: int, the weight of the target relative to your other targets
    """
    body = {}
    body['options'] = options
    body['engines'] = engines
    assert type(engines) == list
    body['stage'] = stage
    body['weight'] = weight
    url = 'https://'+login_cc+'/targets'
    global auth_token
    headers = {'Authorization': auth_token}
    reply = requests.post(url, data=json.dumps(body),
                          verify=is_domain(login_cc), headers=headers)
    if reply.status_code != 200:
        print(reply.status_code, reply.text)
        raise Exception('Cannot add target')
    target_id = reply.json()['target_id']
    target = Target(target_id)
    return target

load_target = Target
load_stream = Stream


@require_login
def get_targets():
    """ Return a list of targets. """
    global login_cc
    url = 'https://'+login_cc+'/targets'
    reply = requests.get(url, verify=is_domain(login_cc))
    if reply.status_code != 200:
        raise Exception('Cannot list targets')
    target_ids = reply.json()['targets']
    targets = set()
    for target_id in target_ids:
        targets.add(Target(target_id))
    return targets
