#!/bin/bash

docker compose down -v

sudo rm -rf bionicpro-auth/init.sql/ ldap/data/*

docker compose up -d --build --force-recreate
