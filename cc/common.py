# Tornado-powered workserver backend.
#
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

import subprocess
import sys
import time
import tornado
import ipaddress
import os
import motor
import tornado.options
import tornado.web
import logging
import psutil
import signal

from pymongo.read_preferences import ReadPreference


def kill_children():
    p = psutil.Process(os.getpid())
    child_pid = p.get_children(recursive=True)
    for pid in child_pid:
        if pid.name() == p.name():
            os.kill(pid.pid, signal.SIGTERM)


def is_domain(url):
    """ Returns True if url is a domain """
    # if a port is present in the url, then we extract the base part
    base = url.split(':')[0]
    if base == 'localhost':
        return False
    try:
        ipaddress.ip_address(base)
        return False
    except Exception:
        return True


def preexec():  # Don't forward signals.
    os.setpgrp()

class CommonHandler(tornado.web.RequestHandler):

    def set_default_headers(self):
        self.set_header("Access-Control-Allow-Origin", "*")

    @property
    def motor(self):
        return self.application.motor

    @tornado.gen.coroutine
    def get_current_user(self, token=None):
        """" Get the user making the request. If token is None, then this
        method will use the request's Authorization header. """
        if token is None:
            try:
                header_token = self.request.headers['Authorization']
            except KeyError:
                return None
        # TODO: Add a caching mechanism of tokens to user ids
        cursor = self.motor.users.all
        query = yield cursor.find_one({'token': header_token}, fields=['_id'])
        if query:
            return query['_id']
        else:
            return None

    @tornado.gen.coroutine
    def is_admin(self, user):
        if user is None:
            return False
        cursor = self.motor.users.admins
        query = yield cursor.find_one({'_id': user})
        if query:
            return True
        else:
            return False

    @tornado.gen.coroutine
    def is_manager(self, user):
        if user is None:
            return False
        cursor = self.motor.users.managers
        query = yield cursor.find_one({'_id': user})
        if query:
            return True
        else:
            return False

    def write_error(self, status_code, **kwargs):
        exception = kwargs['exc_info'][1]
        if isinstance(exception, tornado.web.HTTPError):
            self.write({'error': exception.reason})

    def error(self, message='', code=400):
        """ Write a message to the output buffer and flush. """
        raise tornado.web.HTTPError(code, reason=message)


class BaseServerMixin():

    def initialize_motor(self):
        mongo_options = self._mongo_options
        if is_domain(mongo_options['host']):
            mongo_options['ssl'] = True
        if 'replicaSet' in mongo_options:
            self.motor = motor.MotorReplicaSetClient(**mongo_options)
            self.motor.read_preference = ReadPreference.SECONDARY_PREFERRED
        else:
            self.motor = motor.MotorClient(**mongo_options)

    def base_init(self, name, redis_options, mongo_options):
        """ A BaseServer is a server that is connected to both a redis server
        and a mongo server """
        self.name = name
        self.data_folder = name+'_data'
        if not os.path.exists(self.data_folder):
            os.makedirs(self.data_folder)
        access_channel = logging.FileHandler(os.path.join(self.data_folder,
            'access.log'))
        logging.getLogger('tornado.access').addHandler(access_channel)
        app_channel = logging.FileHandler(os.path.join(self.data_folder,
            'application.log'))
        logging.getLogger('tornado.application').addHandler(app_channel)
        general_channel = logging.FileHandler(os.path.join(self.data_folder,
            'general.log'))
        logging.getLogger('tornado.general').addHandler(general_channel)
        self._mongo_options = mongo_options

    def shutdown(self):
        tornado.ioloop.IOLoop.instance().stop()


def configure_options(config_file, extra_options=None):
    if extra_options:
        for option_name, option_type in extra_options.items():
            tornado.options.define(option_name, type=option_type)
    tornado.options.define('name', type=str)
    tornado.options.define('redis_options', type=dict)
    tornado.options.define('mongo_options', type=dict)
    tornado.options.define('internal_http_port', type=int)
    tornado.options.define('ssl_certfile', type=str, default='')
    tornado.options.define('ssl_key', type=str, default='')
    tornado.options.define('ssl_ca_certs', type=str, default='')
    tornado.options.define('external_host', type=str)
    tornado.options.define('num_processes', type=int, default=0)
    tornado.options.parse_config_file(config_file)
