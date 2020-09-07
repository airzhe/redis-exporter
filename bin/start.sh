#!/bin/bash

LOG=../logs

set -e

cd `dirname $0`

mkdir -p ../logs

chmod +x redis-exporter-mon
chmod +x redis-exporter
chmod +x run.sh

nohup ./redis-exporter-mon -d -l  $LOG/redis-exporter-mon.log  ./run.sh &>$LOG/nohup.log