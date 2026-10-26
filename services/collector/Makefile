# trooper-agent — run from services/collector
export GOWORK ?= off

.PHONY: build test vet itest itest-up itest-down docker

build:
	go build ./...

vet:
	go vet ./... && go vet -tags integration ./...

test:
	go test ./...

# Integration tests: real Postgres / MySQL / Redis / RabbitMQ in ttcol-* containers +
# MongoDB 8 (single-node replica set) + Apache Kafka 4 (KIP-848) + the shared Redpanda at
# localhost:19092 / :9644 (topics/groups prefixed ttcol-).
itest-up:
	docker run -d --name ttcol-postgres -e POSTGRES_PASSWORD=ttcol -p 55432:5432 postgres:16-alpine \
	  -c shared_preload_libraries=pg_stat_statements -c pg_stat_statements.track=all
	docker run -d --name ttcol-mysql -e MYSQL_ROOT_PASSWORD=ttcol -p 53306:3306 mysql:8.4
	docker run -d --name ttcol-redis -p 56379:6379 redis:7-alpine
	docker run -d --name ttcol-rabbitmq -e RABBITMQ_DEFAULT_USER=ttcol -e RABBITMQ_DEFAULT_PASS=ttcol \
	  -p 55672:15672 -p 55673:5672 -p 55692:15692 rabbitmq:3.13-management-alpine
	docker run -d --name ttcol-mongo -p 57017:27017 mongo:8 --replSet rs0 --bind_ip_all   # rs initiated by the tests
	docker run -d --name ttcol-kafka4 -p 59092:9092 -e KAFKA_NODE_ID=1 -e KAFKA_PROCESS_ROLES=broker,controller \
	  -e KAFKA_LISTENERS=PLAINTEXT://:9092,CONTROLLER://:9093 -e KAFKA_ADVERTISED_LISTENERS=PLAINTEXT://localhost:59092 \
	  -e KAFKA_CONTROLLER_LISTENER_NAMES=CONTROLLER -e KAFKA_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT \
	  -e KAFKA_CONTROLLER_QUORUM_VOTERS=1@localhost:9093 -e KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR=1 \
	  -e KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR=1 -e KAFKA_TRANSACTION_STATE_LOG_MIN_ISR=1 \
	  -e KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS=0 apache/kafka:4.1.0   # KIP-848 groups (test skips when absent)

itest:
	go test -tags integration -count=1 ./...

itest-down:
	-docker rm -f ttcol-postgres ttcol-mysql ttcol-redis ttcol-rabbitmq ttcol-mongo ttcol-kafka4

docker:
	docker build -f Dockerfile -t trooper-agent:dev ..
