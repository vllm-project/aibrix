import argparse
from melange.lib.runner import SolverRunner


def main(config_path: str):
    runner = SolverRunner(config_path)
    runner.run()
    runner.export()

if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    # Input arguments
    parser.add_argument(
        "--config",
        "-c",
        type=str,
        default="melange/config/example.json",
        help="Path to the input configuration file, in json",
    )
    args = parser.parse_args()

    main(args.config)
