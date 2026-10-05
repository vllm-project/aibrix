#!/usr/bin/env python3
"""
Command-line utility for generating Kubernetes secrets for S3 and TOS storage.

This script provides a convenient way to create storage secrets in Kubernetes
clusters using the secret_gen module.
"""

import argparse
import sys
from pathlib import Path

# Add the parent directory to the path so we can import aibrix modules
sys.path.insert(0, str(Path(__file__).parent.parent))

from aibrix.metadata.secret_gen import SecretGenerator


def create_s3_secret_cli(args):
    """Create an S3 secret from CLI arguments."""
    try:
        if not args.bucket:
            print("❌ Error: Bucket name is required for S3 secret creation.")
            sys.exit(1)

        generator = SecretGenerator(namespace=args.namespace)

        secret_name = generator.create_s3_secret(
            bucket_name=args.bucket, secret_name=args.name
        )

        print(f"✅ Successfully created S3 secret: {secret_name}")
        print(f"   Namespace: {args.namespace}")
        print(f"   Bucket: {args.bucket}")

    except Exception as e:
        print(f"❌ Failed to create S3 secret: {e}")
        sys.exit(1)


def create_tos_secret_cli(args):
    """Create a TOS secret from CLI arguments."""
    try:
        if not args.bucket:
            print("❌ Error: Bucket name is required for TOS secret creation.")
            sys.exit(1)

        generator = SecretGenerator(namespace=args.namespace)

        secret_name = generator.create_tos_secret(
            bucket_name=args.bucket, secret_name=args.name
        )

        print(f"✅ Successfully created TOS secret: {secret_name}")
        print(f"   Namespace: {args.namespace}")
        print(f"   Bucket: {args.bucket}")

    except Exception as e:
        print(f"❌ Failed to create TOS secret: {e}")
        sys.exit(1)


def delete_secret_cli(args):
    """Delete a secret from CLI arguments."""
    try:
        generator = SecretGenerator(namespace=args.namespace)

        if generator.delete_secret(args.secret_name):
            print(f"✅ Successfully deleted secret: {args.secret_name}")
            print(f"   Namespace: {args.namespace}")
        else:
            print(f"⚠️ Secret not found: {args.secret_name}")
            print(f"   Namespace: {args.namespace}")

    except Exception as e:
        print(f"❌ Failed to delete secret: {e}")
        sys.exit(1)


def list_secrets_cli(args):
    """List secrets in the namespace."""
    try:
        generator = SecretGenerator(namespace=args.namespace)

        # Get all secrets in the namespace
        secrets = generator.core_v1.list_namespaced_secret(namespace=args.namespace)

        print(f"📋 Secrets in namespace '{args.namespace}':")
        print("-" * 50)

        if not secrets.items:
            print("   No secrets found")
        else:
            for secret in secrets.items:
                secret_type = secret.type or "Opaque"
                data_keys = list(secret.data.keys()) if secret.data else []
                print(f"   {secret.metadata.name} ({secret_type})")
                if data_keys:
                    print(f"     Keys: {', '.join(data_keys)}")

    except Exception as e:
        print(f"❌ Failed to list secrets: {e}")
        sys.exit(1)


def main():
    """Main CLI function."""
    parser = argparse.ArgumentParser(
        description="Generate Kubernetes secrets for S3 and TOS storage",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  # Create S3 secret with default name
  python -m scripts.generate_secrets s3 --bucket my-bucket

  # Create S3 secret with custom name
  python -m scripts.generate_secrets s3 --bucket my-bucket --name my-s3-creds

  # Create TOS secret (requires TOS_* environment variables)
  python -m scripts.generate_secrets tos --bucket my-tos-bucket

  # Delete a secret
  python -m scripts.generate_secrets delete my-secret-name

  # List all secrets in namespace
  python -m scripts.generate_secrets list

  # Use custom namespace (either position works)
  python -m scripts.generate_secrets --namespace my-namespace s3 --bucket my-bucket
  python -m scripts.generate_secrets s3 --bucket my-bucket --namespace my-namespace
        """,
    )

    parser.add_argument(
        "--namespace",
        "-n",
        default="default",
        help="Kubernetes namespace (default: default)",
    )

    subparsers = parser.add_subparsers(dest="command", help="Available commands")

    # S3 secret command
    s3_parser = subparsers.add_parser("s3", help="Create S3 secret")
    s3_parser.add_argument("--bucket", "-b", help="S3 bucket name (optional)")
    s3_parser.add_argument(
        "--name", help="Custom secret name (optional, uses template default)"
    )
    s3_parser.add_argument(
        "--namespace",
        "-n",
        default="default",
        help="Kubernetes namespace (default: default)",
    )

    # TOS secret command
    tos_parser = subparsers.add_parser("tos", help="Create TOS secret")
    tos_parser.add_argument("--bucket", "-b", help="TOS bucket name (optional)")
    tos_parser.add_argument(
        "--name", help="Custom secret name (optional, uses template default)"
    )
    tos_parser.add_argument(
        "--namespace",
        "-n",
        default="default",
        help="Kubernetes namespace (default: default)",
    )

    # Delete secret command
    delete_parser = subparsers.add_parser("delete", help="Delete a secret")
    delete_parser.add_argument("secret_name", help="Name of the secret to delete")
    delete_parser.add_argument(
        "--namespace",
        "-n",
        default="default",
        help="Kubernetes namespace (default: default)",
    )

    # List secrets command
    list_parser = subparsers.add_parser("list", help="List secrets in namespace")
    list_parser.add_argument(
        "--namespace",
        "-n",
        default="default",
        help="Kubernetes namespace (default: default)",
    )

    args = parser.parse_args()

    if not args.command:
        parser.print_help()
        sys.exit(1)

    # Handle namespace argument - use subparser's namespace if provided, otherwise use main parser's
    # This allows --namespace to work in either position
    if hasattr(args, "namespace") and args.namespace != "default":
        # Subparser namespace takes priority
        namespace = args.namespace
    else:
        # Fall back to main parser namespace (this handles the case where --namespace comes before subcommand)
        namespace = getattr(args, "namespace", "default")

    # Update args.namespace to the resolved value
    args.namespace = namespace

    # Check Kubernetes access
    try:
        from kubernetes import config

        config.load_kube_config()
    except Exception as e:
        print(f"❌ Failed to load Kubernetes configuration: {e}")
        print("Make sure you have kubectl configured and access to a cluster")
        sys.exit(1)

    # Execute the appropriate command
    if args.command == "s3":
        create_s3_secret_cli(args)
    elif args.command == "tos":
        create_tos_secret_cli(args)
    elif args.command == "delete":
        delete_secret_cli(args)
    elif args.command == "list":
        list_secrets_cli(args)


if __name__ == "__main__":
    main()
