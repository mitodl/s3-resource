FROM golang:1.6.3-alpine

ENV CONCOURSE_CODE_PATH ${GOPATH}/src/github.com/concourse/s3-resource

RUN apk add --update git bash \
  && rm -rf /var/cache/apk/*

ADD . /code

RUN mkdir -p $(dirname ${CONCOURSE_CODE_PATH}) \
    && ln -s /code ${CONCOURSE_CODE_PATH} \
    && cd ${CONCOURSE_CODE_PATH} \
    && ./scripts/build \
    && mkdir -p /opt/resource \
    && cp assets/check /opt/resource/check \
    && cp assets/in /opt/resource/in \
    && cp assets/out /opt/resource/out \
    && cd / \
    && rm -rf ${GOPATH} /code
