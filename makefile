
build:
	go build -buildvcs=false -o airport-tracker.exe .

run:
	./airport-tracker.exe -addr :8080 -fids-debug