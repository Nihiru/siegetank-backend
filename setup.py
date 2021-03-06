# we do not distribute the server code via pypi, ever. So instead, we use the
# pypi file here to distribute the siegetank API code.

from distutils.core import setup

setup(name='siegetank',
      version='0.6',
      description='Siegetank Python Driver',
      author='Yutong Zhao',
      author_email='proteneer@gmail.com',
      url='http://www.proteneer.com',
      license='MIT',
      packages=['siegetank'],
      )
