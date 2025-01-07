# Nomad Wait
Wait for Hashicorp Nomad allocations.

Since the Nomad CLI supports monitoring deployments with `nomad job run`, this tool provides similar functionality for dispatched jobs and is intended as a temporary solution until issue https://github.com/hashicorp/nomad/issues/11601 is resolved.

[![GitHub issue/pull request detail](https://img.shields.io/github/issues/detail/state/hashicorp/nomad/11601)](https://github.com/hashicorp/nomad/issues/11601)


## Usage

`nomad-wait [args] <job>`

## Arguments

- `address` - The address of the Nomad server.
              Overrides the NOMAD_ADDR environment variable if set. 
              Default is `http://127.0.0.1:4646`
- `-t` `-timeout` - Wait timeout in seconds.
                    Overrides the NOMAD_JOB_TIMEOUT environment variable if set.
                    Default is `60`

## Environment variables

- `NOMAD_ADDR` - Nomad server address.
                 Default is `http://127.0.0.1:4646`
- `NOMAD_JOB_TIMEOUT` - Wait timeout in seconds.
                        Default is `60`
