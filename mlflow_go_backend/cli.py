import json
import pathlib
import shlex

import click
import mlflow.cli
import mlflow.server
import mlflow.version
from mlflow.utils import find_free_port

from mlflow_go_backend.lib import get_lib


def _get_commands():
    """Returns the MLflow CLI commands with the `server` command replaced with a Go server."""
    commands = mlflow.cli.cli.commands.copy()

    def server(
        go_opts,
        **kwargs,
    ):
        # convert the Go options to a dictionary
        opts = {}
        if go_opts:
            for opt in go_opts.split(","):
                key, value = opt.split("=", 1)
                opts[key] = value

        # validate the Python server configuration if set
        if ("python_address" in opts) ^ ("python_command" in opts):
            raise click.ClickException("python_address and python_command have to be set together")

        if "python_address" and "python_command" in opts:
            # use the provided Python server configuration
            python_address = opts["python_address"]
            python_command = shlex.split(opts["python_command"])
        else:
            # assign a random port for the Python server
            python_host = "127.0.0.1"
            python_port = find_free_port()
            python_address = f"{python_host}:{python_port}"
            python_args = kwargs.copy()
            python_args.update(
                {
                    "host": python_host,
                    "port": python_port,
                }
            )

            # construct the Python server command
            python_command = [
                "mlflow",
                "server",
            ]
            for key, value in python_args.items():
                if isinstance(value, bool):
                    if value:
                        python_command.append(f"--{key.replace('_', '-')}")
                elif value is not None:
                    python_command.append(f"--{key.replace('_', '-')}")
                    python_command.append(str(value))

        # initialize the Go server configuration
        tracking_store_uri = kwargs["backend_store_uri"]
        config = {
            "address": f'{kwargs["host"]}:{kwargs["port"]}',
            "default_artifact_root": mlflow.cli.resolve_default_artifact_root(
                kwargs["serve_artifacts"], kwargs["default_artifact_root"], tracking_store_uri
            ),
            "log_level": opts.get("log_level", "DEBUG" if kwargs["dev"] else "INFO"),
            "python_address": python_address,
            "python_command": python_command,
            "shutdown_timeout": opts.get("shutdown_timeout", "1m"),
            "static_folder": pathlib.Path(mlflow.server.__file__)
            .parent.joinpath(mlflow.server.REL_STATIC_DIR)
            .resolve()
            .as_posix(),
            "tracking_store_uri": tracking_store_uri,
            "model_registry_store_uri": kwargs["registry_store_uri"] or tracking_store_uri,
            "version": mlflow.version.VERSION,
        }
        config_bytes = json.dumps(config).encode("utf-8")

        # start the Go server and check for errors
        ret = get_lib().LaunchServer(config_bytes, len(config_bytes))
        if ret != 0:
            raise click.ClickException(f"Non-zero exit code: {ret}")

    server.__doc__ = mlflow.cli.server.callback.__doc__

    server_params = mlflow.cli.server.params.copy()
    idx = next((i for i, x in enumerate(mlflow.cli.server.params) if x.name == "gunicorn_opts"), -1)
    server_params.insert(
        idx,
        click.Option(
            ["--go-opts"],
            default=None,
            help="Additional options forwarded to the Go server",
        ),
    )

    server_command = click.command(params=server_params)(server)
    commands["server"] = server_command
    # In mlflow `ui` is an alias of `server`.
    commands["ui"] = server_command

    return commands


@click.group(commands=_get_commands())
def cli():
    pass


if __name__ == "__main__":
    cli()
