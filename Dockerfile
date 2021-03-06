FROM scratch
LABEL maintainer="Gregor Riepl <onitake@gmail.com>"

COPY restreamer /
COPY examples/minimal/restreamer.json /

EXPOSE 8000
CMD ["/restreamer", "/restreamer.json"]
