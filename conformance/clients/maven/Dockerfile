# Maven client with the full jar-lifecycle plugin closure baked in at image
# build time, so conformance runs stay hermetic: scenario-time downloads go
# only through the registry under test.
FROM maven:3-eclipse-temurin-21
COPY warmup-pom.xml /warmup/pom.xml
RUN mkdir -p /warmup/src/main/java/com/example \
    && printf 'package com.example;\npublic class Warmup {}\n' > /warmup/src/main/java/com/example/Warmup.java \
    && mvn -B -q -f /warmup/pom.xml \
       org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve \
    && mvn -B -q -f /warmup/pom.xml deploy \
    # _remote.repositories pins each cached artifact to the repository id it
    # came from; removing it lets the warmed cache serve any mirror id.
    && find /root/.m2 -name "_remote.repositories" -delete \
    && rm -rf /warmup /tmp/warmup-repo
