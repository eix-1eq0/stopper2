go mod tidy     # fetches golang.org/x/term and writes go.sum
go build -o stopper .
./stopper -v
./stopper -o mylog.log
