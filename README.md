# Nomad Wait
Monitor for Hashicorp Nomad batch jobs

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
