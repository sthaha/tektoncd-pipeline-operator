FROM alpine:3.8

ENV OPERATOR=/usr/local/bin/tektoncd-pipeline-operator \
    USER_UID=1001 \
    USER_NAME=tektoncd-pipeline-operator

# install operator binary
COPY build/_output/bin/tektoncd-pipeline-operator ${OPERATOR}

COPY build/bin /usr/local/bin
RUN  /usr/local/bin/user_setup

COPY deploy/resources /data/resources

ENTRYPOINT ["/usr/local/bin/entrypoint"]

USER ${USER_UID}
