import unittest
import os
import shutil
import sys

import tornado
import tornado.web
import tornado.httpclient
import tornado.testing
import tornado.gen

import ws
import cc

import base64
import os
import json


class Handler1(tornado.web.RequestHandler):
    def get(self):
        self.write("One")


class Handler2(tornado.web.RequestHandler):
    @tornado.gen.coroutine
    def get(self):
        client = tornado.httpclient.AsyncHTTPClient()
        response = yield client.fetch('http://localhost:8000/one')
        self.write("%s plus Two" % response.body)


class Test(tornado.testing.AsyncTestCase):

    @classmethod
    def setUpClass(cls):
        super(Test, cls).setUpClass()
        cls.ws_rport = 2398
        cls.cc_rport = 5872
        cls.ws_hport = 9028
        cls.cc_hport = 8342
        cls.ws = ws.WorkServer('mengsk', redis_port=cls.ws_rport)
        cls.cc = cc.CommandCenter('goliath', redis_port=cls.cc_rport)

    def setUp(self):
        super(Test, self).setUp()
        self.cc.add_ws('mengsk', '127.0.0.1', self.ws_hport, self.ws_rport)
        self.cc.listen(self.cc_hport, io_loop=self.io_loop, ssl_options={
            'certfile': 'certs/ws.crt', 'keyfile': 'certs/ws.key'})
        self.ws.listen(self.ws_hport, io_loop=self.io_loop, ssl_options={
            'certfile': 'certs/cc.crt', 'keyfile': 'certs/cc.key'})

    def test_post_target_and_streams(self):
        client = tornado.httpclient.AsyncHTTPClient(io_loop=self.io_loop)
        fb1, fb2, fb3, fb4 = (base64.b64encode(os.urandom(1024)).decode()
                              for i in range(4))
        description = "Diwakar and John's top secret project"
        body = {
            'description': description,
            'files': {'system.xml.gz.b64': fb1, 'integrator.xml.gz.b64': fb2},
            'steps_per_frame': 50000,
            'engine': 'openmm',
            'engine_versions': ['6.0'],
            }
        url = '127.0.0.1'
        uri = 'https://'+url+':'+str(self.cc_hport)+'/targets'
        client.fetch(uri, self.stop, method='POST', body=json.dumps(body),
                     validate_cert=cc._is_domain(url))
        reply = self.wait()
        self.assertEqual(reply.code, 200)
        target_id = json.loads(reply.body.decode())['target_id']
        body = {'target_id': target_id,
                'files': {"state.xml.gz.b64": fb3}
                }
        uri = 'https://'+url+':'+str(self.cc_hport)+'/streams'
        client.fetch(uri, self.stop, method='POST', body=json.dumps(body),
                     validate_cert=cc._is_domain(url))
        reply = self.wait()
        self.assertEqual(reply.code, 200)

    @classmethod
    def tearDown(cls):
        super(Test, cls).tearDownClass()
        cls.cc.db.flushdb()
        cls.cc.shutdown_redis()
        cls.ws.db.flushdb()
        cls.ws.shutdown_redis()
        folders = ['streams', 'targets']
        for folder in folders:
            if os.path.exists(folder):
                shutil.rmtree(folder)

if __name__ == '__main__':
    unittest.main()
