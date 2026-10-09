.PHONY: proto test up down sim

# Regenerates courier.pb.go / courier_grpc.pb.go from courier.proto and
# overwrites the committed files. Run this deliberately after editing the
# .proto, then commit the result. Uses the buildenv image so a local protoc
# install isn't required.
proto:
	docker build -f Dockerfile.buildenv -t route-buildenv .
	docker run --rm -v "$(CURDIR):/app" -w /app route-buildenv \
		protoc --go_out=. --go_opt=paths=source_relative \
		       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
		       internal/api/proto/courier.proto

# Runs the full build+test cycle in the buildenv container against a
# throwaway postgres/redis (see the `test` profile in docker-compose.yml).
test:
	# Recreate the throwaway databases first: init.sql only runs on a fresh
	# container, so a leftover one from before a schema change fails every test.
	docker-compose -f deploy/compose/docker-compose.yml --profile test down
	docker-compose -f deploy/compose/docker-compose.yml --profile test run --rm test
	docker-compose -f deploy/compose/docker-compose.yml --profile test down

up:
	docker-compose -f deploy/compose/docker-compose.yml up -d --build postgres redis redpanda routed prometheus grafana

down:
	docker-compose -f deploy/compose/docker-compose.yml down

# make sim                      the plain demo load
# make sim PROFILE=demo          a scenario from loadtest/scenarios
sim:
	SIM_PROFILE=$(if $(PROFILE),/scenarios/$(PROFILE).json,) docker-compose -f deploy/compose/docker-compose.yml up --build -d routesim
