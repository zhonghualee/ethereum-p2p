#!/bin/bash
# check if root
if [ "$EUID" -ne 0 ]; then
  echo "please run as root"
  exit 1
fi
if [ "$#" -ne 1 ]; then
  echo "argument missing"
  echo "usage: ./run.sh scale-size"
  exit 1
fi

n=$(( $1 - 1 ))
WORKING_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
MYSQL_NAME="ethnodes-mysql"
NODEFINDER_NAME="node-finder"
# remove existing instances
docker rm -f $(docker ps -a | egrep -o "${MYSQL_NAME}-[0-9]+")
docker rm -f $(docker ps -a | egrep -o "${NODEFINDER_NAME}-[0-9]+")

# build mysql container image
MYSQL_IMAGE="mysql:5.7-ethnodes"
docker build -t ${MYSQL_IMAGE} -f ethnodes-dockerfile .

# get env variables
source .env

# run mysql containers
MYSQL_PORT=3300
echo "starting mysql containers..."
for i in `seq 0 ${n}`;
do
  MYSQL_DIR="${ROOT_DIR}-${i}/ethnodes"
  BACKUP_DIR="${ROOT_DIR}-${i}/ethnodes-backup"
  mkdir -p ${BACKUP_DIR}
  chmod 777 ${BACKUP_DIR}
  PORT=$(( ${MYSQL_PORT}+${i} ))
  docker run -d --restart=always -p ${PORT}:3306 -h ${MYSQL_NAME}-${i} --name ${MYSQL_NAME}-${i} \
    --env MYSQL_DATABASE=${MYSQL_DB} \
    --env MYSQL_ROOT_PASSWORD=${MYSQL_PASSWORD} \
    --env MYSQL_USER=${MYSQL_USERNAME} \
    --env MYSQL_PASSWORD=${MYSQL_PASSWORD} \
    -v ${WORKING_DIR}/configs:/etc/mysql/conf.d \
    -v ${MYSQL_DIR}:/var/lib/mysql \
    -v ${BACKUP_DIR}:/backup \
    ${MYSQL_IMAGE}
  echo "${MYSQL_NAME}-${i} started"
done

read -p "Press any key to continue... "
# make node-finder container image
cd ${WORKING_DIR}/..
NODEFINDER_IMAGE="geth:node-finder"
docker build -t ${NODEFINDER_IMAGE} .

# run node-finders
URL="research-scan.sprai.org"
NODEFINDER_PORT=30310
DATADIR="/root/.ethereum"
LOGFILE="${DATADIR}/${NODEFINDER_NAME}.log"
echo "starting node-finder containers..."
for i in `seq 0 ${n}`;
do
  IDENTITY="uiuc-${NODEFINDER_NAME}-${i} (${URL})"
  NODEFINDER_DIR="${ROOT_DIR}-${i}/ethereum"
  MYSQL_URL="${MYSQL_USERNAME}:${MYSQL_PASSWORD}@tcp(${MYSQL_HOST}:$(( ${MYSQL_PORT}+${i} )))/${MYSQL_DB}"
  PORT=$(( ${NODEFINDER_PORT}+${i} ))
  CMD="geth \
    --identity \"${IDENTITY}\" \
    --datadir \"${DATADIR}\" \
    --port ${PORT} \
    --mysql \"${MYSQL_URL}\" \
    --nomaxpeers \
    --verbosity 5 \
    --dialfreq 1800 \
    --maxnumfile 20480 \
    --backupsql 2>&1 | while read line; do \
      echo \${line} >> ${DATADIR}/${NODEFINDER_NAME}.log; \
      LOG=\$(echo \${line} | egrep '(\|\[.+\]\||Dial task done|Failed proto handshake)' | cut -d'|' -f3- | sed 's/[A-Za-z]*=//g'); \
      echo \${LOG} | grep 'NEIGH' | cut -d'|' -f3- >> ${DATADIR}/${NODEFINDER_NAME}-neighbors.bsv; \
      echo \${LOG} | grep 'HELLO' | cut -d'|' -f3- >> ${DATADIR}/${NODEFINDER_NAME}-hello.bsv; \
      echo \${LOG} | grep '\-PROT' | cut -d'|' -f3- >> ${DATADIR}/${NODEFINDER_NAME}-disc-proto.bsv; \
      echo \${LOG} | grep '\-PEER' | cut -d'|' -f3- >> ${DATADIR}/${NODEFINDER_NAME}-disc-peer.bsv; \
      echo \${LOG} | grep 'STATU' | cut -d'|' -f3- >> ${DATADIR}/${NODEFINDER_NAME}-status.bsv; \
      echo \${LOG} | grep 'DAOFO' | cut -d'|' -f3- >> ${DATADIR}/${NODEFINDER_NAME}-daofork.bsv; \
      echo \${LOG} | egrep 'done\|(static|dyn)' | cut -d'|' -f1,3 | \
        sed 's/[ ]/|/g' | awk -F'|' '{print \$1 \"|\" \$3 \"|\" \$4 \"|\" \$2}' \
        >> ${DATADIR}/${NODEFINDER_NAME}-dial-done.bsv; \
      echo \${LOG} | grep 'done|discovery' | sed 's/[ ]/|/g;s/(//' | cut -d'|' -f1,7 \
        >> ${DATADIR}/${NODEFINDER_NAME}-discovery-done.bsv; \
      echo \${LOG} | grep 'Failed' | awk -F'|' '{print \$1 \"|\" \$3 \"|\" \$4 \"|\" \$5 \"|\" \$6}' \
        >> ${DATADIR}/${NODEFINDER_NAME}-failed-handshake.bsv; \
    done"
  docker run -d --restart=always -h ${NODEFINDER_NAME}-${i} --name ${NODEFINDER_NAME}-${i} -p ${PORT}:${PORT} -p ${PORT}:${PORT}/udp -v ${NODEFINDER_DIR}:${DATADIR} -e CMD="${CMD}" --entrypoint '/bin/sh' ${NODEFINDER_IMAGE} -c "${CMD}"
 echo "${NODEFINDER_NAME}-${i} started"
done