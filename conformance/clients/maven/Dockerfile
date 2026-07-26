# Maven client with the dependency-plugin closure baked in at image build
# time, so conformance runs stay hermetic: scenario-time downloads go only
# through the registry under test.
FROM maven:3-eclipse-temurin-21
COPY warmup-pom.xml /warmup/pom.xml
# Full plugin coordinates (no prefix metadata lookup), and _remote.repositories
# tracking files removed so the warmed cache is usable regardless of which
# repository id the scenario's mirror uses.
RUN mvn -B -q -f /warmup/pom.xml org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve \
    && find /root/.m2 -name "_remote.repositories" -delete \
    && rm -rf /warmup
