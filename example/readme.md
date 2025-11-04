This example demonstrates how a platform with generic APIs could be implemented.

The platform here is a simplified version of an Internal Developer
Platform (IDP) that provides generic APIs for managing resources.

In a real-world scenario, the platform could be implemented using [kcp](https://kcp.io/), with the clusters `consumer`, `db`, and `cloud` being kcp workspaces.

The script `run.bash` sets up the environment and steps through the example.

The premise is that the consumer is an internal team that wants to
deploy an application that requires a database. For this the platform
offers the generic API `pgs.example.platform-mesh.io`, which the
consumer uses to request a PostgreSQL database.

In the initial development the database does not need to be highly
available or store large amounts of data, hence the platform schedules
the database in the `db` provider, which offers small in-house
databases.

Later the requirements change and the consumer needs a highly available
database that can store large amounts of data. The platform then
schedules the database in the `cloud` provider, which offers managed
cloud databases.

<!--

TODO

The platform deploys workloads to migrate the data from the old database
in the `db` provider to the new database in the `cloud` provider.

After the migration is complete the consumer switches to using the new
database and the platform deletes the old database.

-->

## kind clusters

- platform:
  The cluster used to run workloads for the platform.
- consumer:
  The workload cluster of the consumer.
- db:
  A cluster providing small databases.
- cloud:
  A cluster simulating a cloud provider environment.
