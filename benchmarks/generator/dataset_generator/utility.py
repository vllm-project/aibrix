import argparse
from generator.dataset_generator.converter import process_dataset_trace, process_dataset_sharegpt

def convert(args):
    if args.type == 'trace':
        process_dataset_trace(args.path, args.output)
    elif args.type == 'sharegpt':
        process_dataset_sharegpt(args.path, args.output)
    else:
        raise ValueError(f"Unknown type: {args.type}")

    
def main(args):
    convert(args)
    

if __name__ == "__main__":
    parser = argparse.ArgumentParser(prog="utility", description="Multi-tool utility")
    subparsers = parser.add_subparsers(dest="command", required=True)

    # Subcommand: convert
    parser_convert = subparsers.add_parser("convert", help="Convert a dataset")
    parser_convert.add_argument("--path", required=True, help="Input dataset path")
    parser_convert.add_argument('--type', type=str, required=True, choices=['trace', 'sharegpt'],
                                help='Dataset type: trace or sharegpt')
    parser_convert.add_argument("--output", type=str, default="output.jsonl", help="Output file name.")
    parser_convert.set_defaults(func=convert)
    
    args = parser.parse_args()
    print(f"Arguments: {args}")
    main(args)